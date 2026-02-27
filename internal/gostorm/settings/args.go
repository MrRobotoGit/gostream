package settings

type ExecArgs struct {
	Port        string
	IP          string
	Ssl         bool
	SslPort     string
	SslCert     string
	SslKey      string
	Path        string
	LogPath     string
	WebLogPath  string
	RDB         bool
	HttpAuth    bool
	TorrentsDir string
	TorrentAddr string
	PubIPv4     string
	PubIPv6     string
	SearchWA    bool
	MaxSize     string

	ProxyURL  string
	ProxyMode string
}

var Args *ExecArgs
