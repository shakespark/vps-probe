package release

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const agentYML = "node: \"hk-1\"\nserver:\n  addr: \"probe.example.com:9527\"\n  token: \"it's a 'quoted' $token `x`\"\n"

func TestBuiltInSigners(t *testing.T) {
	lines, principal, err := signerLines(Signers)
	if err != nil || len(lines) == 0 || principal != "shakespark" {
		t.Fatalf("lines %v principal %q err %v", lines, principal, err)
	}
	for _, l := range lines {
		if !strings.Contains(l, `namespaces="`+Namespace+`"`) {
			t.Errorf("key not limited to the release namespace: %s", l)
		}
	}
}

func TestLine(t *testing.T) {
	line, err := Command{Version: "0.1.17", AgentConfig: agentYML}.Line()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, " sh -c '") || !strings.HasSuffix(line, "'") || strings.Count(line, "'") != 2 || strings.Contains(line, "\n") {
		t.Fatalf("not one single-quoted line: %s", line)
	}
	for _, want := range []string{"V=0.1.17", "B=" + DefaultBase + "/v$V", "-n " + Namespace, "-I shakespark",
		base64.StdEncoding.EncodeToString([]byte(agentYML)), "install.sh agent --config agent.yml"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", line).CombinedOutput(); err != nil {
		t.Fatalf("not valid sh: %v %s", err, out)
	}

	for name, c := range map[string]Command{
		"dev version":        {Version: "dev", AgentConfig: agentYML},
		"shell in version":   {Version: "1.0.0; rm -rf /", AgentConfig: agentYML},
		"quote in base":      {Version: "1.0.0", Base: "https://x/'; id; '", AgentConfig: agentYML},
		"space in base":      {Version: "1.0.0", Base: "https://x/a b", AgentConfig: agentYML},
		"not a URL":          {Version: "1.0.0", Base: "$(id)", AgentConfig: agentYML},
		"no config":          {Version: "1.0.0"},
		"no key":             {Version: "1.0.0", AgentConfig: agentYML, Signers: "# nothing\n"},
		"two principals":     {Version: "1.0.0", AgentConfig: agentYML, Signers: "a ssh-ed25519 AAAA\nb ssh-ed25519 BBBB\n"},
		"shell in principal": {Version: "1.0.0", AgentConfig: agentYML, Signers: "a;id ssh-ed25519 AAAA\n"},
	} {
		if line, err := c.Line(); err == nil {
			t.Errorf("%s: accepted: %s", name, line)
		}
	}
}

// fakeRelease serves a release whose install.sh records its arguments and
// the config it was given, signed with a throwaway key.
type fakeRelease struct {
	dir, signers, out string
	srv               *httptest.Server
}

func newFakeRelease(t *testing.T, version string) *fakeRelease {
	t.Helper()
	for _, tool := range []string{"sh", "ssh-keygen", "sha256sum", "tar", "base64", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	f := &fakeRelease{dir: dir, out: filepath.Join(dir, "installed")}
	sums := ""
	for _, arch := range []string{"amd64", "arm64"} {
		pkg := fmt.Sprintf("vps-probe-%s-linux-%s", version, arch)
		os.Mkdir(filepath.Join(dir, pkg), 0o755)
		stub := "#!/bin/sh\necho \"$*\" > \"$FAKE_OUT.args\"\ncp \"$3\" \"$FAKE_OUT\"\n"
		if err := os.WriteFile(filepath.Join(dir, pkg, "install.sh"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		run("tar", "czf", pkg+".tar.gz", pkg)
		b, _ := os.ReadFile(filepath.Join(dir, pkg+".tar.gz"))
		sums += fmt.Sprintf("%x  %s.tar.gz\n", sha256.Sum256(b), pkg)
	}
	sumFile := "vps-probe-" + version + ".sha256"
	os.WriteFile(filepath.Join(dir, sumFile), []byte(sums), 0o644)
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "key")
	run("ssh-keygen", "-q", "-Y", "sign", "-f", "key", "-n", Namespace, sumFile)
	pub, _ := os.ReadFile(filepath.Join(dir, "key.pub"))
	p := strings.Fields(string(pub))
	f.signers = "# test key\ntester namespaces=\"" + Namespace + "\" " + p[0] + " " + p[1] + "\n"
	f.srv = httptest.NewServer(http.StripPrefix("/v"+version, http.FileServer(http.Dir(dir))))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) install(t *testing.T, c Command) (string, error) {
	t.Helper()
	c.Base = f.srv.URL
	line, err := c.Line()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command("sh", "-c", line)
	// No proxy between the test and its own server.
	cmd.Env = append(os.Environ(), "FAKE_OUT="+f.out, "NO_PROXY=*", "no_proxy=*", "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	// Success or not, the work directory with the token is gone.
	if left, _ := os.ReadDir(tmp); len(left) > 0 {
		t.Errorf("left behind in the temp dir: %v", left)
	}
	return string(out), err
}

func TestInstallEndToEnd(t *testing.T) {
	f := newFakeRelease(t, "9.9.9")
	out, err := f.install(t, Command{Version: "9.9.9", Signers: f.signers, AgentConfig: agentYML})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := os.ReadFile(f.out)
	if string(got) != agentYML {
		t.Fatalf("install.sh got this config:\n%s", got)
	}
	if args, _ := os.ReadFile(f.out + ".args"); strings.TrimSpace(string(args)) != "agent --config agent.yml" {
		t.Fatalf("install.sh args: %s", args)
	}
}

func TestInstallRefusesUnverified(t *testing.T) {
	other := newFakeRelease(t, "9.9.9") // only for its key
	for name, tamper := range map[string]func(f *fakeRelease) Command{
		"signed by another key": func(f *fakeRelease) Command {
			return Command{Version: "9.9.9", Signers: other.signers, AgentConfig: agentYML}
		},
		"built-in keys": func(f *fakeRelease) Command {
			return Command{Version: "9.9.9", AgentConfig: agentYML}
		},
		"tarball changed after signing": func(f *fakeRelease) Command {
			for _, a := range []string{"amd64", "arm64"} {
				p := filepath.Join(f.dir, "vps-probe-9.9.9-linux-"+a+".tar.gz")
				b, _ := os.ReadFile(p)
				os.WriteFile(p, append(b, 0), 0o644)
			}
			return Command{Version: "9.9.9", Signers: f.signers, AgentConfig: agentYML}
		},
		"checksum file changed after signing": func(f *fakeRelease) Command {
			p := filepath.Join(f.dir, "vps-probe-9.9.9.sha256")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, append(b, '\n'), 0o644)
			return Command{Version: "9.9.9", Signers: f.signers, AgentConfig: agentYML}
		},
		"tarball missing from the checksum file": func(f *fakeRelease) Command {
			// Re-sign a checksum file without any tarball line.
			os.WriteFile(filepath.Join(f.dir, "vps-probe-9.9.9.sha256"), []byte("0000  other.tar.gz\n"), 0o644)
			os.Remove(filepath.Join(f.dir, "vps-probe-9.9.9.sha256.sig"))
			cmd := exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", "key", "-n", Namespace, "vps-probe-9.9.9.sha256")
			cmd.Dir = f.dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			return Command{Version: "9.9.9", Signers: f.signers, AgentConfig: agentYML}
		},
		"release does not exist": func(f *fakeRelease) Command {
			return Command{Version: "9.9.8", Signers: f.signers, AgentConfig: agentYML}
		},
	} {
		f := newFakeRelease(t, "9.9.9")
		out, err := f.install(t, tamper(f))
		if err == nil {
			t.Errorf("%s: the command succeeded:\n%s", name, out)
		}
		if _, statErr := os.Stat(f.out + ".args"); statErr == nil {
			t.Errorf("%s: install.sh ran", name)
		}
	}
}
