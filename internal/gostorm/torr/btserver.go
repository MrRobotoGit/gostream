package torr

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/publicip"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	"golang.org/x/time/rate"

	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr/storage/torrstor"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr/utils"
	"github.com/MrRobotoGit/gostream/internal/gostorm/version"
)

type BTServer struct {
	config *torrent.ClientConfig
	client *torrent.Client

	storage *torrstor.Storage

	torrents map[metainfo.Hash]*Torrent

	mu sync.Mutex
}

var privateIPBlocks []*net.IPNet

// DHT Bootstrap nodes per discovery veloce
// Questi nodi sono contattati immediatamente all'avvio per popolare la DHT
var dhtBootstrapHosts = []string{
	// Nodi funzionanti (verificati)
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.libtorrent.org:25401",
	"dht.aelitis.com:6881",

	// Aggiunti per ridondanza
	"router.bitcomet.com:6881",
	"dht.vuze.com:6881",
}

// Cache per evitare ri-risoluzione DNS ad ogni torrent
var resolvedDhtNodes []dht.Addr
var dhtNodesOnce sync.Once

// resolveDhtBootstrapNodes risolve i nodi DHT una sola volta all'avvio
func resolveDhtBootstrapNodes() []dht.Addr {
	dhtNodesOnce.Do(func() {
		log.Println("DHT Bootstrap: resolving", len(dhtBootstrapHosts), "hosts...")
		startTime := time.Now()

		for _, hostPort := range dhtBootstrapHosts {
			host, portStr, err := net.SplitHostPort(hostPort)
			if err != nil {
				log.Printf("DHT Bootstrap: invalid host:port %s: %v", hostPort, err)
				continue
			}

			// Risolvi DNS con timeout
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			var resolver net.Resolver
			ips, err := resolver.LookupIP(ctx, "ip4", host)
			cancel()

			if err != nil {
				log.Printf("DHT Bootstrap: DNS failed for %s: %v", host, err)
				continue
			}

			port, _ := net.LookupPort("udp", portStr)
			for _, ip := range ips {
				if ip.To4() != nil {
					addr := dht.NewAddr(&net.UDPAddr{IP: ip, Port: port})
					resolvedDhtNodes = append(resolvedDhtNodes, addr)
				}
			}
		}

		log.Printf("DHT Bootstrap: resolved %d addresses in %v",
			len(resolvedDhtNodes), time.Since(startTime).Round(time.Millisecond))
	})

	return resolvedDhtNodes
}

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // RFC3927 link-local
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local addr
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Errorf("parse error on %q: %v", cidr, err))
		}
		privateIPBlocks = append(privateIPBlocks, block)
	}
}

func NewBTS() *BTServer {
	bts := new(BTServer)
	bts.torrents = make(map[metainfo.Hash]*Torrent)
	return bts
}

func (bt *BTServer) Connect() error {
	bt.mu.Lock()
	var err error
	bt.configure(context.TODO())
	bt.client, err = torrent.NewClient(bt.config)
	bt.torrents = make(map[metainfo.Hash]*Torrent)
	bt.mu.Unlock()

	// V1.4.0: Align anacrolix reader max readahead with configured CacheSize
	torrent.SetMaxReadahead(settings.BTsets.CacheSize)

	// V227: InitApiHelper takes btsMu.Lock — must be called OUTSIDE bt.mu
	// to prevent AB/BA deadlock with SetSettings (btsMu → bt.mu)
	InitApiHelper(bt)
	go bt.StartTicker()
	return err
}

// V143-Audit: StartTicker runs a central heartbeat for all torrents
// Replaces individual per-torrent tickers to prevent "Thundering Herd"
func (bt *BTServer) StartTicker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		bt.mu.Lock()
		if bt.client == nil {
			bt.mu.Unlock()
			return
		}

		// Snapshot list to avoid holding lock during updates
		list := make([]*Torrent, 0, len(bt.torrents))
		for _, t := range bt.torrents {
			list = append(list, t)
		}
		bt.mu.Unlock()

		// Update each torrent concurrently or sequentially?
		// Sequentially is safer for CPU spikes, and UpdateStats is fast.
		for _, t := range list {
			t.UpdateStats()
		}
	}
}

func (bt *BTServer) Disconnect() {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.client != nil {
		bt.client.Close()
		bt.client = nil
		utils.FreeOSMemGC()
	}
}

func (bt *BTServer) configure(ctx context.Context) {
	blocklist, _ := utils.ReadBlockedIP()
	bt.config = torrent.NewDefaultClientConfig()

	bt.storage = torrstor.NewStorage(settings.BTsets.CacheSize)
	bt.config.DefaultStorage = bt.storage

	userAgent := "qBittorrent/5.0.3"
	peerID := "-qB5030-"
	upnpID := "GoStorm/" + version.Version
	cliVers := userAgent

	bt.config.Debug = settings.BTsets.EnableDebug
	bt.config.DisableIPv6 = !settings.BTsets.EnableIPv6
	bt.config.DisableTCP = settings.BTsets.DisableTCP
	bt.config.DisableUTP = settings.BTsets.DisableUTP
	//	https://github.com/anacrolix/torrent/issues/703
	// bt.config.DisableWebtorrent = true //	NE
	// bt.config.DisableWebseeds = false  //	NE
	bt.config.NoDefaultPortForwarding = settings.BTsets.DisableUPNP
	bt.config.NoDHT = settings.BTsets.DisableDHT
	bt.config.DisablePEX = settings.BTsets.DisablePEX

	// NUOVO: DHT Bootstrap Nodes espliciti per discovery veloce
	if !settings.BTsets.DisableDHT {
		bt.config.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
			return func() ([]dht.Addr, error) {
				addrs := resolveDhtBootstrapNodes()
				if len(addrs) == 0 {
					// Fallback ai nodi default della libreria
					log.Println("DHT Bootstrap: using library defaults (resolution failed)")
					return dht.GlobalBootstrapAddrs(network)
				}
				return addrs, nil
			}
		}
	}

	bt.config.NoUpload = settings.BTsets.DisableUpload
	bt.config.IPBlocklist = blocklist
	bt.config.Bep20 = peerID
	bt.config.PeerID = utils.PeerIDRandom(peerID)
	bt.config.UpnpID = upnpID
	bt.config.HTTPUserAgent = userAgent
	bt.config.ExtendedHandshakeClientVersion = cliVers
	bt.config.EstablishedConnsPerTorrent = 35 // V266: Increased from 25 for better seeder discovery
	// V87-Balanced-Discovery: Optimized for Pi + home router
	// Balance fast discovery with system safety
	bt.config.TotalHalfOpenConns = 500          // V264: Was 800, reduced for Pi 4 stability
	bt.config.HalfOpenConnsPerTorrent = 60      // V266: Increased from 40 for faster startup
	bt.config.DisableAcceptRateLimiting = false // V262: Enabled rate limiting to discard unauthorized/aggressive peers

	// V230: Piece hashers per torrent (v1.55 default: 2, v1.22 used NumCPU=4)
	bt.config.PieceHashersPerTorrent = 2

	// V266: Double dial rate for aggressive discovery (default: 10/10, doubled in V264, now 40/40)
	bt.config.DialRateLimiter = rate.NewLimiter(40, 40)

	// V262: Balanced Peer Reaping & Hardening
	// 1. KeepAliveTimeout: Send pings every 30s (was 20s). Balance between discovery and cleanup.
	// 2. HandshakesTimeout: 5s (was 3s) to allow peers from thin swarms under VPN latency.
	// 3. HighWater/LowWater: 200/50 — V266: HighWater increased to store more candidates from big swarms.
	bt.config.KeepAliveTimeout = 30 * time.Second
	bt.config.HandshakesTimeout = 5 * time.Second
	bt.config.TorrentPeersHighWater = 200 // V266: Was 40
	bt.config.TorrentPeersLowWater = 50   // V266: Was 25
	bt.config.DropMutuallyCompletePeers = true

	// V93-Aggressive-Discovery: Reduce timeouts to filter slow peers quickly
	bt.config.NominalDialTimeout = 6 * time.Second // Balance: 3s -> 6s (Stability)
	bt.config.MinDialTimeout = 2 * time.Second     // Balance: 1s -> 2s (Stability)
	//
	// Reasoning:
	// - 800 total: Optimal discovery vs CPU load on Pi 4
	// - 5s/7s timeouts: Necessary to account for ProtonVPN latency overhead
	// - EstablishedConnsPerTorrent=25 maintained (CONFIRMED OPTIMAL: 217 Mbps)
	// Encryption/Obfuscation
	bt.config.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
		RequirePreferred: settings.BTsets.ForceEncrypt,
		Preferred:        settings.BTsets.ForceEncrypt, // Fixed: use ForceEncrypt for both to match logic
	}
	if settings.BTsets.ForceEncrypt {
		bt.config.CryptoProvides = mse.CryptoMethodRC4
	} else {
		bt.config.CryptoProvides = mse.AllSupportedCrypto
	}
	if settings.BTsets.DownloadRateLimit > 0 {
		bt.config.DownloadRateLimiter = utils.Limit(settings.BTsets.DownloadRateLimit * 1024)
	}
	if settings.BTsets.UploadRateLimit > 0 {
		bt.config.UploadRateLimiter = utils.Limit(settings.BTsets.UploadRateLimit * 1024)
	}
	if settings.TorAddr != "" {
		log.Println("Set listen addr", settings.TorAddr)
		bt.config.SetListenAddr(settings.TorAddr)
	} else {
		if settings.BTsets.PeersListenPort > 0 {
			log.Println("Set listen port", settings.BTsets.PeersListenPort)
			bt.config.ListenPort = settings.BTsets.PeersListenPort
		} else {
			log.Println("Set listen port to random autoselect (0)")
			bt.config.ListenPort = 0
		}
	}

	// Configure proxy if enabled
	if err := bt.configureProxy(); err != nil {
		log.Println("Proxy configuration error:", err)
	}

	log.Println("Client config:", settings.BTsets)

	var err error

	// set public IPv4
	if settings.PubIPv4 != "" {
		if ip4 := net.ParseIP(settings.PubIPv4); ip4.To4() != nil && !isPrivateIP(ip4) {
			bt.config.PublicIp4 = ip4
		}
	}
	if bt.config.PublicIp4 == nil {
		bt.config.PublicIp4, err = publicip.Get4(ctx)
		if err != nil {
			log.Printf("error getting public ipv4 address: %v", err)
		}
	}
	if bt.config.PublicIp4.To4() == nil { // possible IPv6 from publicip.Get4(ctx)
		bt.config.PublicIp4 = nil
	}
	if bt.config.PublicIp4 != nil {
		log.Println("PublicIp4:", bt.config.PublicIp4)
	}

	// set public IPv6
	if settings.PubIPv6 != "" {
		if ip6 := net.ParseIP(settings.PubIPv6); ip6.To16() != nil && ip6.To4() == nil && !isPrivateIP(ip6) {
			bt.config.PublicIp6 = ip6
		}
	}
	if bt.config.PublicIp6 == nil && settings.BTsets.EnableIPv6 {
		bt.config.PublicIp6, err = publicip.Get6(ctx)
		if err != nil {
			log.Printf("error getting public ipv6 address: %v", err)
		}
	}
	if bt.config.PublicIp6.To16() == nil { // just 4 sure it's valid IPv6
		bt.config.PublicIp6 = nil
	}
	if bt.config.PublicIp6 != nil {
		log.Println("PublicIp6:", bt.config.PublicIp6)
	}
}

func (bt *BTServer) configureProxy() error {
	proxyURL := settings.Args.ProxyURL

	if proxyURL == "" {
		return nil // No proxy configured
	}

	proxyMode := settings.Args.ProxyMode
	if proxyMode == "" {
		proxyMode = "tracker" // default
	}

	// Parse and validate proxy URL
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}

	scheme := parsedURL.Scheme
	// Validate proxy protocol
	switch scheme {
	case "socks5", "socks5h", "socks4", "socks4a", "http", "https":
		// Supported protocols
	default:
		return fmt.Errorf("unsupported proxy protocol: %s (supported: http, https, socks4, socks4a, socks5, socks5h)", scheme)
	}

	if proxyMode == "full" {
		log.Printf("Configuring proxy for all BitTorrent traffic: %s://%s", scheme, parsedURL.Host)

		// Set ProxyURL - REMOVED for v1.55.0 compatibility
		// bt.config.ProxyURL = proxyURL

		// Also set HTTPProxy explicitly for HTTP tracker requests
		bt.config.HTTPProxy = func(req *http.Request) (*url.URL, error) {
			return parsedURL, nil
		}

		log.Println("Proxy configured successfully for all BitTorrent connections (tracker, DHT, peers) - NOTE: Peer proxy might be limited in v1.55.0")
	} else if proxyMode == "peers" {
		log.Printf("Configuring proxy for peer connections only: %s://%s", scheme, parsedURL.Host)

		// Set ProxyURL for peer connections - REMOVED for v1.55.0 compatibility
		// bt.config.ProxyURL = proxyURL

		log.Println("Proxy configured successfully for peer and DHT connections only - NOTE: Peer proxy might be limited in v1.55.0")
	} else {
		log.Printf("Configuring proxy for HTTP tracker requests only: %s://%s", scheme, parsedURL.Host)

		// Only set HTTPProxy for tracker requests, don't set ProxyURL
		bt.config.HTTPProxy = func(req *http.Request) (*url.URL, error) {
			return parsedURL, nil
		}

		log.Println("Proxy configured successfully for HTTP tracker connections only")
	}

	return nil
}

func (bt *BTServer) GetTorrent(hash torrent.InfoHash) *Torrent {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if torr, ok := bt.torrents[hash]; ok {
		return torr
	}
	return nil
}

func (bt *BTServer) ListTorrents() map[metainfo.Hash]*Torrent {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	// V239-Optimization: Pre-allocate map size to avoid continuous resizing
	list := make(map[metainfo.Hash]*Torrent, len(bt.torrents))
	maps.Copy(list, bt.torrents)
	return list
}

func (bt *BTServer) RemoveTorrent(hash torrent.InfoHash) bool {
	bt.mu.Lock()
	torr, ok := bt.torrents[hash]
	bt.mu.Unlock()
	if ok {
		return torr.Close()
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}
