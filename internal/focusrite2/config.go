package focusrite2

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"

	"github.com/sebastianrau/sweetspot/internal/domain"
)

// Config contains the trusted server and the local, persistent client identity.
// PrivateKey must only be stored in the owner-readable settings file.
type Config struct {
	DeviceSerial    string                            `yaml:"device_serial,omitempty"`
	MasterMute      ControlRef                        `yaml:"master_mute,omitempty"`
	MasterDim       ControlRef                        `yaml:"master_dim,omitempty"`
	Outputs         [domain.NumSpeakers]OutputBinding `yaml:"outputs,omitempty"`
	Host            string                            `yaml:"host,omitempty"`
	OnboardingPort  int                               `yaml:"onboarding_port,omitempty"`
	SecurePort      int                               `yaml:"secure_port,omitempty"`
	ServerPublicKey string                            `yaml:"server_public_key,omitempty"`
	PrivateKey      string                            `yaml:"private_key,omitempty"`
}

func (c Config) address(port int) string {
	host := c.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return "ws://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}
func key(s string) ([32]byte, error) {
	var k [32]byte
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != 32 {
		return k, errors.New("FC2 requires a 32-byte hexadecimal key")
	}
	copy(k[:], b)
	return k, nil
}
func (c Config) identity() (*ecdh.PrivateKey, error) {
	b, e := key(c.PrivateKey)
	if e != nil {
		return nil, e
	}
	return ecdh.X25519().NewPrivateKey(b[:])
}
func (c Config) Validate() error {
	if c.OnboardingPort < 1 || c.OnboardingPort > 65535 || c.SecurePort < 1 || c.SecurePort > 65535 {
		return errors.New("FC2 ports must be between 1 and 65535")
	}
	_, e := key(c.ServerPublicKey)
	return e
}

// AdoptPairing takes the client identity and the trusted server (address,
// ports, public key) of a successful pairing. Mappings and the device serial
// stay as they are.
func (c Config) AdoptPairing(paired Config) Config {
	c.PrivateKey = paired.PrivateKey
	c.ServerPublicKey = paired.ServerPublicKey
	c.Host = paired.Host
	c.OnboardingPort = paired.OnboardingPort
	c.SecurePort = paired.SecurePort
	return c
}

func (c Config) WithIdentity() (Config, error) {
	if c.PrivateKey != "" {
		_, e := c.identity()
		return c, e
	}
	p, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e == nil {
		c.PrivateKey = hex.EncodeToString(p.Bytes())
	}
	return c, e
}

var ErrIdentityChanged = errors.New("FC2 server identity changed; pair again")

// Discover only adopts an announcement from this computer. A stored server key
// pins the identity; a changed key requires explicit pairing again.
func Discover(ctx context.Context, c Config) (Config, error) {
	r, e := zeroconf.NewResolver(nil)
	if e != nil {
		return c, e
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry, 16)
	if e = r.Browse(ctx, "_ocaws._tcp", "local.", entries); e != nil {
		return c, e
	}
	for {
		select {
		case <-ctx.Done():
			return c, fmt.Errorf("FC2 discovery: %w", ctx.Err())
		case entry, ok := <-entries:
			if !ok {
				return c, errors.New("FC2 discovery ended")
			}
			local := false
			addrs, _ := net.InterfaceAddrs()
			for _, a := range addrs {
				ip, _, _ := net.ParseCIDR(a.String())
				for _, remote := range append(entry.AddrIPv4, entry.AddrIPv6...) {
					if ip.Equal(remote) {
						local = true
					}
				}
			}
			if !local {
				continue
			}
			fields := map[string]string{}
			for _, v := range entry.Text {
				k, v, ok := strings.Cut(v, "=")
				if ok {
					fields[k] = v
				}
			}
			if _, e := key(fields["publicKey"]); e != nil {
				continue
			}
			port, e := strconv.Atoi(fields["securePort"])
			if e != nil || port < 1 || port > 65535 {
				continue
			}
			if c.ServerPublicKey != "" && !strings.EqualFold(c.ServerPublicKey, fields["publicKey"]) {
				return c, ErrIdentityChanged
			}
			c.Host = "127.0.0.1"
			c.OnboardingPort = entry.Port
			c.SecurePort = port
			c.ServerPublicKey = fields["publicKey"]
			return c, c.Validate()
		}
	}
}

// ControlRef binds a selected object to its semantic path as well as its number.
// A changed device layout is rediscovered, never silently written with old IDs.
type ControlRef struct {
	Number uint32 `yaml:"number,omitempty"`
	Path   string `yaml:"path,omitempty"`
}
type OutputBinding struct {
	Name       string     `yaml:"name,omitempty"` // unused: names come from FC2
	Gain       ControlRef `yaml:"gain,omitempty"`
	Mute       ControlRef `yaml:"mute,omitempty"`
	GainRight  ControlRef `yaml:"gain_right,omitempty"`
	MuteRight  ControlRef `yaml:"mute_right,omitempty"`
	MeterLeft  ControlRef `yaml:"meter_left,omitempty"`
	MeterRight ControlRef `yaml:"meter_right,omitempty"`
}
