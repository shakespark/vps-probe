package netaddr

import "testing"

func TestValid(t *testing.T) {
	for s, want := range map[string][3]bool{ // host, host:port, listen
		"203.0.113.5":       {true, false, false},
		"2001:db8::1":       {true, false, false},
		"probe.example.com": {true, false, false},
		"a_b.example.com":   {false, false, false},
		"-a.example.com":    {false, false, false},
		"":                  {false, false, false},
		"203.0.113.5:9527":  {false, true, true},
		"[2001:db8::1]:53":  {false, true, true},
		"example.com:0":     {false, false, true},
		"example.com:65536": {false, false, false},
		":9527":             {false, false, true},
		"bad host:1":        {false, false, false},
	} {
		if got := [3]bool{ValidHost(s), ValidHostPort(s), ValidListen(s)}; got != want {
			t.Errorf("%q: host, host:port, listen = %v, want %v", s, got, want)
		}
	}
}
