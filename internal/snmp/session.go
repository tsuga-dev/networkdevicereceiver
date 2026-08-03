package snmp

import (
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Session is the transport the fetcher talks to. It exists as an interface so
// the fetch strategy, which is where the awkward device behaviour lives, can be
// tested against a fake agent without a network.
type Session interface {
	Connect() error
	Close() error
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	GetBulk(oids []string, maxRepetitions uint32) (*gosnmp.SnmpPacket, error)
	GetNext(oids []string) (*gosnmp.SnmpPacket, error)
	// SupportsGetBulk is false for SNMPv1, which has no GETBULK.
	SupportsGetBulk() bool
}

// Credentials describe how to authenticate to a device. One set is tried at a
// time during discovery; the set that answers is remembered per device.
type Credentials struct {
	Version string `mapstructure:"version"`

	// Community is the v1/v2c community string.
	Community string `mapstructure:"community"`

	// v3 USM parameters.
	User         string `mapstructure:"user"`
	AuthProtocol string `mapstructure:"auth_protocol"`
	AuthKey      string `mapstructure:"auth_key"`
	PrivProtocol string `mapstructure:"priv_protocol"`
	PrivKey      string `mapstructure:"priv_key"`
	ContextName  string `mapstructure:"context_name"`
}

// ConnectionConfig is everything needed to open a session to one device.
type ConnectionConfig struct {
	Host string
	Port uint16

	Credentials Credentials

	Timeout time.Duration
	Retries int
}

// Validate checks the credential set is internally coherent, so a
// misconfiguration is reported at startup rather than as a silent auth failure
// against every device in a subnet.
func (c Credentials) Validate() error {
	version, err := c.snmpVersion()
	if err != nil {
		return err
	}
	if version == gosnmp.Version3 {
		if c.User == "" {
			return fmt.Errorf("snmp v3 requires user")
		}
		if _, err := authProtocol(c.AuthProtocol); err != nil {
			return err
		}
		if _, err := privProtocol(c.PrivProtocol); err != nil {
			return err
		}
		if c.PrivKey != "" && c.AuthKey == "" {
			return fmt.Errorf("snmp v3 privacy requires an auth_key")
		}
		if c.PrivProtocol != "" && c.PrivKey == "" {
			return fmt.Errorf("priv_protocol %q set without priv_key", c.PrivProtocol)
		}
		if c.AuthProtocol != "" && c.AuthKey == "" {
			return fmt.Errorf("auth_protocol %q set without auth_key", c.AuthProtocol)
		}
		return nil
	}
	if c.Community == "" {
		return fmt.Errorf("snmp %s requires community", c.Version)
	}
	return nil
}

func (c Credentials) snmpVersion() (gosnmp.SnmpVersion, error) {
	switch strings.ToLower(strings.TrimPrefix(c.Version, "v")) {
	case "1":
		return gosnmp.Version1, nil
	case "2c", "2":
		return gosnmp.Version2c, nil
	case "3":
		return gosnmp.Version3, nil
	case "":
		// v2c is the pragmatic default: universally supported and, unlike v1,
		// it has GETBULK and 64-bit counters.
		return gosnmp.Version2c, nil
	default:
		return 0, fmt.Errorf("unsupported snmp version %q", c.Version)
	}
}

// securityLevel derives the USM level from which secrets are present, so users
// do not have to state it separately and cannot state it inconsistently.
func (c Credentials) securityLevel() gosnmp.SnmpV3MsgFlags {
	switch {
	case c.AuthKey != "" && c.PrivKey != "":
		return gosnmp.AuthPriv
	case c.AuthKey != "":
		return gosnmp.AuthNoPriv
	default:
		return gosnmp.NoAuthNoPriv
	}
}

func authProtocol(name string) (gosnmp.SnmpV3AuthProtocol, error) {
	switch strings.ToUpper(strings.ReplaceAll(name, "-", "")) {
	case "":
		return gosnmp.NoAuth, nil
	case "MD5":
		return gosnmp.MD5, nil
	case "SHA", "SHA1":
		return gosnmp.SHA, nil
	case "SHA224":
		return gosnmp.SHA224, nil
	case "SHA256":
		return gosnmp.SHA256, nil
	case "SHA384":
		return gosnmp.SHA384, nil
	case "SHA512":
		return gosnmp.SHA512, nil
	default:
		return 0, fmt.Errorf("unsupported auth protocol %q", name)
	}
}

func privProtocol(name string) (gosnmp.SnmpV3PrivProtocol, error) {
	switch strings.ToUpper(strings.ReplaceAll(name, "-", "")) {
	case "":
		return gosnmp.NoPriv, nil
	case "DES":
		return gosnmp.DES, nil
	case "AES", "AES128":
		return gosnmp.AES, nil
	case "AES192":
		return gosnmp.AES192, nil
	case "AES256":
		return gosnmp.AES256, nil
	// The "C" variants use Cisco's non-standard key extension; several vendors
	// only interoperate with these.
	case "AES192C":
		return gosnmp.AES192C, nil
	case "AES256C":
		return gosnmp.AES256C, nil
	default:
		return 0, fmt.Errorf("unsupported priv protocol %q", name)
	}
}

// gosnmpSession is the production Session.
type gosnmpSession struct {
	client *gosnmp.GoSNMP
}

// NewSession builds a session from a connection config. It does not connect.
func NewSession(cfg ConnectionConfig) (Session, error) {
	version, err := cfg.Credentials.snmpVersion()
	if err != nil {
		return nil, err
	}
	if err := cfg.Credentials.Validate(); err != nil {
		return nil, err
	}

	port := cfg.Port
	if port == 0 {
		port = 161
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	client := &gosnmp.GoSNMP{
		Target:    cfg.Host,
		Port:      port,
		Version:   version,
		Timeout:   timeout,
		Retries:   cfg.Retries,
		Transport: "udp",
		// The default is to keep the socket open between polls, which matters
		// when thousands of devices are polled on a short interval.
		ExponentialTimeout: true,
	}

	if version == gosnmp.Version3 {
		auth, err := authProtocol(cfg.Credentials.AuthProtocol)
		if err != nil {
			return nil, err
		}
		priv, err := privProtocol(cfg.Credentials.PrivProtocol)
		if err != nil {
			return nil, err
		}
		client.SecurityModel = gosnmp.UserSecurityModel
		client.MsgFlags = cfg.Credentials.securityLevel()
		client.ContextName = cfg.Credentials.ContextName
		client.SecurityParameters = &gosnmp.UsmSecurityParameters{
			UserName:                 cfg.Credentials.User,
			AuthenticationProtocol:   auth,
			AuthenticationPassphrase: cfg.Credentials.AuthKey,
			PrivacyProtocol:          priv,
			PrivacyPassphrase:        cfg.Credentials.PrivKey,
		}
	} else {
		client.Community = cfg.Credentials.Community
	}

	return &gosnmpSession{client: client}, nil
}

func (s *gosnmpSession) Connect() error { return s.client.Connect() }

func (s *gosnmpSession) Close() error {
	if s.client.Conn == nil {
		return nil
	}
	return s.client.Conn.Close()
}

func (s *gosnmpSession) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	return s.client.Get(oids)
}

func (s *gosnmpSession) GetBulk(oids []string, maxRepetitions uint32) (*gosnmp.SnmpPacket, error) {
	// Non-repeaters is 0: every requested OID is a column to be walked.
	return s.client.GetBulk(oids, 0, maxRepetitions)
}

func (s *gosnmpSession) GetNext(oids []string) (*gosnmp.SnmpPacket, error) {
	return s.client.GetNext(oids)
}

func (s *gosnmpSession) SupportsGetBulk() bool {
	return s.client.Version != gosnmp.Version1
}
