package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/shakespark/vps-probe/internal/server/basicauth"
	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/notify"
	"github.com/shakespark/vps-probe/internal/server/store"
)

// loadConfig parses a command's flags and loads the config they name.
func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	path := configFlag(fs)
	fs.Parse(args)
	return config.Load(*path)
}

func check(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("check", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %d nodes, %d alert rules, %d reports, %d notify channels, ingest udp %s, web %s, db %s\n",
		len(cfg.Nodes), len(cfg.Alerts), len(cfg.Reports), len(cfg.Notify), cfg.Listen.Ingest, cfg.Listen.Web, cfg.DB)
	return nil
}

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("o", "", "output file (must not exist)")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *out == "" {
		return errors.New("backup: -o is required")
	}
	if err := store.Backup(context.Background(), cfg.DB, *out); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	return nil
}

// testNotify sends one message to every configured channel and reports
// each result; a failure here is not retried.
func testNotify(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("test-notify", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	if len(cfg.Notify) == 0 {
		return errors.New("no channel is configured: add one under notify in the config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg := fmt.Sprintf("✅ vps-probe 测试消息\n%d 个节点，%d 条告警规则，%d 项报告\n%s", len(cfg.Nodes), len(cfg.Alerts), len(cfg.Reports),
		time.Now().In(cfg.Location).Format("2006-01-02 15:04:05"))
	failed := 0
	for _, c := range cfg.Notify {
		if err := notify.New(c, slog.Default()).Send(ctx, msg); err != nil {
			failed++
			fmt.Printf("%s: FAILED: %v\n", c.Name, err)
		} else {
			fmt.Printf("%s: sent\n", c.Name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d channel(s) failed", failed)
	}
	return nil
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func genToken([]string) error {
	fmt.Println(newToken())
	return nil
}

// hashPassword prints the bcrypt hash for basic_auth.password_hash. On a
// terminal it asks twice without echo; otherwise it reads one line from
// stdin, so the password never has to appear in the shell history.
func hashPassword([]string) error {
	var pw string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		ask := func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
		a, err := ask("Password: ")
		if err != nil {
			return err
		}
		b, err := ask("Again: ")
		if err != nil {
			return err
		}
		if a != b {
			return errors.New("the two passwords differ")
		}
		pw = a
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read the password from stdin: %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	// bcrypt ignores everything after 72 bytes; refuse rather than truncate.
	if len(pw) < 10 || len(pw) > 72 {
		return errors.New("want a password of 10 to 72 bytes")
	}
	h, err := basicauth.Hash(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}
