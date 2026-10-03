package echo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "echo.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfig(t *testing.T) {
	const key = "abcdefghijklmnopqrstuvwxyz0123456789"
	c, err := LoadConfig(write(t, "key: "+key+"\nallow: [203.0.113.7, 198.51.100.9/24]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":39527" || len(c.Prefixes) != 2 || c.Prefixes[0].String() != "203.0.113.7/32" || c.Prefixes[1].String() != "198.51.100.0/24" {
		t.Fatalf("config: %+v", c)
	}
	for name, bad := range map[string]string{
		"short key":   "key: short\n",
		"bad listen":  "key: " + key + "\nlisten: 39527\n",
		"bad allow":   "key: " + key + "\nallow: [example.com]\n",
		"negative":    "key: " + key + "\nmax_pps: -1\n",
		"unknown key": "key: " + key + "\nmax_rate: 5\n",
	} {
		if _, err := LoadConfig(write(t, bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The example config that ships in the release loads once its key is set.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../deploy/echo.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(write(t, string(data))); err == nil {
		t.Fatal("the placeholder key was accepted")
	}
	filled := strings.Replace(string(data), `"<key>"`, "abcdefghijklmnopqrstuvwxyz0123456789", 1)
	filled = strings.Replace(filled, "# allow:", "allow:", 1)
	filled = strings.Replace(filled, "# max_pps:", "max_pps:", 1)
	c, err := LoadConfig(write(t, filled))
	if err != nil || len(c.Prefixes) != 2 || c.MaxPPS != 1000 {
		t.Fatalf("example: %+v, %v", c, err)
	}
}
