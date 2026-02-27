package main

import (
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr"

	natpmp "github.com/jackpal/go-nat-pmp"
)

// Global atomic variable to track current external port for metrics (V229)
var currentNatPort int64

// NatPMPConfig holds the configuration for NAT-PMP port forwarding.
type NatPMPConfig struct {
	Enabled      bool   `json:"enabled"`
	Gateway      string `json:"gateway"`
	LocalPort    int    `json:"local_port"`
	VPNInterface string `json:"vpn_interface"`
	Lifetime     int    `json:"lifetime"`
	Refresh      int    `json:"refresh"`
}

// natpmpLoop runs the NAT-PMP port forwarding loop as a sidecar goroutine.
// It requests a port mapping from the gateway, sets up iptables rules, and
// updates GoStorm's PeersListenPort when the external port changes.
func natpmpLoop(stopChan <-chan struct{}, cfg NatPMPConfig, logger *log.Logger) {
	if !cfg.Enabled {
		return
	}

	// Apply defaults — Gateway must be set in config.json when natpmp.enabled=true
	if cfg.Gateway == "" {
		return
	}
	if cfg.LocalPort == 0 {
		cfg.LocalPort = 8091
	}
	if cfg.VPNInterface == "" {
		cfg.VPNInterface = "wg0"
	}
	if cfg.Lifetime == 0 {
		cfg.Lifetime = 60
	}
	if cfg.Refresh == 0 {
		cfg.Refresh = 45
	}

	logger.Printf("[NatPMP] Starting — gateway=%s interface=%s localPort=%d lifetime=%ds refresh=%ds",
		cfg.Gateway, cfg.VPNInterface, cfg.LocalPort, cfg.Lifetime, cfg.Refresh)

	// Initialize from current GoStorm port to avoid unnecessary SetSettings on startup
	currentExternalPort := 0
	iptablesReady := false // Ensure iptables rules are created on first successful mapping
	if settings.BTsets != nil && settings.BTsets.PeersListenPort > 0 {
		currentExternalPort = settings.BTsets.PeersListenPort
		atomic.StoreInt64(&currentNatPort, int64(currentExternalPort))
		logger.Printf("[NatPMP] Initialized with current GoStorm port: %d", currentExternalPort)
	}

	for {
		// Check VPN interface exists
		_, err := net.InterfaceByName(cfg.VPNInterface)
		if err != nil {
			logger.Printf("[NatPMP] WARNING: interface %s not found: %v — retrying in 10s", cfg.VPNInterface, err)
			select {
			case <-stopChan:
				logger.Println("[NatPMP] Stopped (interface wait)")
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		// Parse gateway IP
		gatewayIP := net.ParseIP(cfg.Gateway)
		if gatewayIP == nil {
			logger.Printf("[NatPMP] ERROR: invalid gateway IP %q — retrying in 10s", cfg.Gateway)
			select {
			case <-stopChan:
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		// Create NAT-PMP client
		client := natpmp.NewClientWithTimeout(gatewayIP, 120*time.Second)

		// Request TCP mapping: internalPort=localPort, requestedExternalPort=currentExternalPort (0 on first request = let gateway choose)
		tcpResult, err := client.AddPortMapping("tcp", cfg.LocalPort, currentExternalPort, cfg.Lifetime)
		if err != nil {
			logger.Printf("[NatPMP] ERROR: TCP mapping failed: %v — retrying in 10s", err)
			select {
			case <-stopChan:
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		externalPort := int(tcpResult.MappedExternalPort)

		// Map UDP with same external port: internalPort=localPort, requestedExternalPort=externalPort
		_, err = client.AddPortMapping("udp", cfg.LocalPort, externalPort, cfg.Lifetime)
		if err != nil {
			logger.Printf("[NatPMP] WARNING: UDP mapping failed: %v (TCP port %d still active)", err, externalPort)
		}

		if externalPort != currentExternalPort {
			logger.Printf("[NatPMP] Port changed: %d → %d", currentExternalPort, externalPort)

			// Clean existing rules first, then add new ones
			cleanupIptablesRules(cfg.VPNInterface, logger)
			setupIptablesRedirect(cfg.VPNInterface, cfg.LocalPort, externalPort, logger)
			iptablesReady = true

			// Update GoStorm port
			updateGoStormPort(externalPort, currentExternalPort, logger)

			currentExternalPort = externalPort
			atomic.StoreInt64(&currentNatPort, int64(externalPort)) // V229: Expose for metrics
		} else if !iptablesReady && externalPort > 0 {
			// First successful mapping after restart — port unchanged but iptables rules are gone
			logger.Printf("[NatPMP] Restoring iptables rules for port %d (post-restart)", externalPort)
			cleanupIptablesRules(cfg.VPNInterface, logger)
			setupIptablesRedirect(cfg.VPNInterface, cfg.LocalPort, externalPort, logger)
			iptablesReady = true
		}

		// Wait for refresh interval or stop signal
		select {
		case <-stopChan:
			// Cleanup on shutdown
			logger.Println("[NatPMP] Shutting down — cleaning up...")
			cleanupIptablesRules(cfg.VPNInterface, logger)
			// Delete mappings (lifetime=0)
			if currentExternalPort > 0 {
				// Delete mappings: lifetime=0, internalPort=0
				client.AddPortMapping("tcp", 0, currentExternalPort, 0)
				client.AddPortMapping("udp", 0, currentExternalPort, 0)
			}
			logger.Println("[NatPMP] Stopped, cleaned up rules and mappings")
			return
		case <-time.After(time.Duration(cfg.Refresh) * time.Second):
			continue
		}
	}
}

// cleanupIptablesRules removes all PREROUTING REDIRECT rules for the given interface.
func cleanupIptablesRules(iface string, logger *log.Logger) {
	out, err := exec.Command("sudo", "iptables", "-t", "nat", "-S", "PREROUTING").CombinedOutput()
	if err != nil {
		logger.Printf("[NatPMP] WARNING: failed to list iptables rules: %v", err)
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, iface) || !strings.Contains(line, "REDIRECT") {
			continue
		}
		// Convert -A to -D for deletion
		if strings.HasPrefix(line, "-A ") {
			deleteRule := strings.Replace(line, "-A ", "-D ", 1)
			args := strings.Fields(deleteRule)
			cmd := exec.Command("sudo", append([]string{"iptables", "-t", "nat"}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				logger.Printf("[NatPMP] WARNING: failed to delete rule: %s — %v", string(out), err)
			}
		}
	}
}

// setupIptablesRedirect adds PREROUTING REDIRECT rules for TCP and UDP.
func setupIptablesRedirect(iface string, fromPort, toPort int, logger *log.Logger) {
	from := strconv.Itoa(fromPort)
	to := strconv.Itoa(toPort)

	for _, proto := range []string{"tcp", "udp"} {
		cmd := exec.Command("sudo", "iptables", "-t", "nat", "-A", "PREROUTING",
			"-i", iface, "-p", proto, "--dport", from, "-j", "REDIRECT", "--to-port", to)
		if out, err := cmd.CombinedOutput(); err != nil {
			logger.Printf("[NatPMP] ERROR: iptables %s redirect failed: %s — %v", proto, string(out), err)
		} else {
			logger.Printf("[NatPMP] iptables: %s %s:%s → :%s", iface, proto, from, to)
		}
	}
}

// updateGoStormPort updates GoStorm's PeersListenPort via direct Go call.
func updateGoStormPort(newPort, oldPort int, logger *log.Logger) {
	currentSets := settings.BTsets
	if currentSets == nil {
		logger.Printf("[NatPMP] WARNING: BTsets is nil, cannot update port")
		return
	}

	if currentSets.PeersListenPort == newPort {
		return
	}

	// Copy current settings and update port
	newSets := *currentSets
	newSets.PeersListenPort = newPort

	logger.Printf("[NatPMP] GoStorm port updating: %d → %d", oldPort, newPort)
	torr.SetSettings(&newSets)
	logger.Printf("[NatPMP] GoStorm port updated: %d → %d (reconnect complete)", oldPort, newPort)
}
