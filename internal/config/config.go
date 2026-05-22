package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server    ServerCfg    `toml:"server"`
	TLS       TLSCfg       `toml:"tls"`
	Log       LogCfg       `toml:"log"`
	RateLimit RateLimitCfg `toml:"ratelimit"`
}

type ServerCfg struct {
	Listen          string `toml:"listen"`
	HealthListen    string `toml:"health_listen"`
	UdpListen       string `toml:"udp_listen"`         // empty = UDP disabled
	UdpAdvertiseHost string `toml:"udp_advertise_host"` // empty = derive from Listen
	MaxRooms        int    `toml:"max_rooms"`
	MaxPeersPerRoom int    `toml:"max_peers_per_room"`
	MaxConnections  int    `toml:"max_connections"`
	AdminToken      string `toml:"admin_token"`

	// Server-level connection password (v1.3+).
	// Empty = no server-level gating (anyone who knows the address can connect).
	// Non-empty = clients must include serverPassword in their hello message;
	// otherwise server replies with SERVER_PASSWORD_REQUIRED / BAD_SERVER_PASSWORD.
	// Stored as plaintext at rest (config.toml is root-only on systemd hosts);
	// hashed in memory on load and only the bcrypt hash is compared per-connection.
	Password string `toml:"password"`
}

type TLSCfg struct {
	Enabled         bool   `toml:"enabled"`
	CertFile        string `toml:"cert_file"`
	KeyFile         string `toml:"key_file"`
	AutoLetsEncrypt bool   `toml:"auto_letsencrypt"`
	Domain          string `toml:"domain"`
	Email           string `toml:"email"`
}

type LogCfg struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
	Path   string `toml:"path"`
}

type RateLimitCfg struct {
	HelloPerIPPerMinute        int `toml:"hello_per_ip_per_minute"`
	RoomCreatePerPeerPerMinute int `toml:"room_create_per_peer_per_minute"`
	RoomJoinPerPeerPerMinute   int `toml:"room_join_per_peer_per_minute"`
	RoomListPerPeerPerMinute   int `toml:"room_list_per_peer_per_minute"`
	AudioFramesPerPeerPerSec   int `toml:"audio_frames_per_peer_per_second"`
}

func Default() Config {
	return Config{
		Server: ServerCfg{
			Listen:           "0.0.0.0:18900",
			HealthListen:     "127.0.0.1:18901",
			UdpListen:        "0.0.0.0:18902",
			UdpAdvertiseHost: "", // empty = use the host the client connected via
			MaxRooms:         50,
			MaxPeersPerRoom:  8,
			MaxConnections:   200,
			AdminToken:       "",
		},
		TLS: TLSCfg{
			Enabled: false,
		},
		Log: LogCfg{
			Level:  "info",
			Format: "json",
			Path:   "",
		},
		RateLimit: RateLimitCfg{
			HelloPerIPPerMinute:        10,
			RoomCreatePerPeerPerMinute: 5,
			RoomJoinPerPeerPerMinute:   30,
			RoomListPerPeerPerMinute:   60,
			AudioFramesPerPeerPerSec:   200,
		},
	}
}

// Load reads a TOML file into a Config, applying defaults for missing fields.
// Returns Default() if path is empty or missing.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		return cfg, nil
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("load config %s: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func validate(c *Config) error {
	if c.Server.Listen == "" {
		return errors.New("server.listen is empty")
	}
	if c.Server.MaxRooms <= 0 {
		c.Server.MaxRooms = 50
	}
	if c.Server.MaxPeersPerRoom <= 0 {
		c.Server.MaxPeersPerRoom = 8
	}
	if c.Server.MaxConnections <= 0 {
		c.Server.MaxConnections = 200
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		c.Log.Level = "info"
	}
	if c.TLS.Enabled && !c.TLS.AutoLetsEncrypt {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("tls.enabled requires cert_file+key_file or auto_letsencrypt+domain")
		}
	}
	if c.TLS.Enabled && c.TLS.AutoLetsEncrypt && c.TLS.Domain == "" {
		return errors.New("tls.auto_letsencrypt requires domain")
	}
	return nil
}
