package snmp

import (
	"strings"
	"testing"
)

func TestCredentialsValidate(t *testing.T) {
	tests := []struct {
		name    string
		creds   Credentials
		wantErr string // substring; empty means it must pass
	}{
		{
			name:  "v2c with community",
			creds: Credentials{Version: "v2c", Community: "public"},
		},
		{
			name:  "version defaults to v2c",
			creds: Credentials{Community: "public"},
		},
		{
			name:    "v2c without community",
			creds:   Credentials{Version: "2c"},
			wantErr: "requires community",
		},
		{
			name:    "unknown version",
			creds:   Credentials{Version: "v4", Community: "public"},
			wantErr: "unsupported snmp version",
		},
		{
			name: "v3 authPriv",
			creds: Credentials{
				Version: "v3", User: "admin",
				AuthProtocol: "SHA256", AuthKey: "authpass",
				PrivProtocol: "AES", PrivKey: "privpass",
			},
		},
		{
			name:  "v3 noAuthNoPriv",
			creds: Credentials{Version: "v3", User: "admin"},
		},
		{
			name:    "v3 without user",
			creds:   Credentials{Version: "v3"},
			wantErr: "requires user",
		},
		{
			name: "v3 privacy without auth is not permitted by USM",
			creds: Credentials{
				Version: "v3", User: "admin",
				PrivProtocol: "AES", PrivKey: "privpass",
			},
			wantErr: "requires an auth_key",
		},
		{
			name: "v3 priv_key without priv_protocol",
			creds: Credentials{
				Version: "v3", User: "admin",
				AuthProtocol: "SHA", AuthKey: "authpass",
				PrivKey: "privpass",
			},
			wantErr: "without priv_protocol",
		},
		{
			name: "v3 auth protocol without key",
			creds: Credentials{
				Version: "v3", User: "admin", AuthProtocol: "SHA256",
			},
			wantErr: "without auth_key",
		},
		{
			name: "v3 unsupported auth protocol",
			creds: Credentials{
				Version: "v3", User: "admin", AuthProtocol: "SHA3", AuthKey: "k",
			},
			wantErr: "unsupported auth protocol",
		},
		{
			name: "v3 unsupported priv protocol",
			creds: Credentials{
				Version: "v3", User: "admin",
				AuthProtocol: "SHA", AuthKey: "k",
				PrivProtocol: "TWOFISH", PrivKey: "p",
			},
			wantErr: "unsupported priv protocol",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.creds.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestAuthAndPrivProtocolAliases covers the spellings that appear in real
// configs, including Cisco's non-standard AES key extension variants.
func TestAuthAndPrivProtocolAliases(t *testing.T) {
	for _, name := range []string{"MD5", "SHA", "SHA1", "sha256", "SHA-256", "SHA384", "SHA512", ""} {
		if _, err := authProtocol(name); err != nil {
			t.Errorf("authProtocol(%q): %v", name, err)
		}
	}
	for _, name := range []string{"DES", "AES", "AES128", "aes192", "AES-256", "AES192C", "AES256C", ""} {
		if _, err := privProtocol(name); err != nil {
			t.Errorf("privProtocol(%q): %v", name, err)
		}
	}
}

func TestSecurityLevelDerivedFromSecrets(t *testing.T) {
	tests := []struct {
		creds Credentials
		want  string
	}{
		{Credentials{}, "NoAuthNoPriv"},
		{Credentials{AuthKey: "a"}, "AuthNoPriv"},
		{Credentials{AuthKey: "a", PrivKey: "p"}, "AuthPriv"},
	}
	for _, tc := range tests {
		if got := tc.creds.securityLevel().String(); got != tc.want {
			t.Errorf("securityLevel(%+v) = %s, want %s", tc.creds, got, tc.want)
		}
	}
}

func TestNewSessionRejectsBadConfig(t *testing.T) {
	if _, err := NewSession(ConnectionConfig{Host: "10.0.0.1"}); err == nil {
		t.Error("expected an error with no community and no v3 user")
	}
	sess, err := NewSession(ConnectionConfig{
		Host:        "10.0.0.1",
		Credentials: Credentials{Version: "v2c", Community: "public"},
	})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if !sess.SupportsGetBulk() {
		t.Error("v2c must support GETBULK")
	}

	v1, err := NewSession(ConnectionConfig{
		Host:        "10.0.0.1",
		Credentials: Credentials{Version: "v1", Community: "public"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v1.SupportsGetBulk() {
		t.Error("v1 has no GETBULK")
	}
}
