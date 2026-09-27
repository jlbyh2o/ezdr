package zrepl

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// zrepl's apt repository and its signing key fingerprint, from
// https://zrepl.github.io/installation/apt-repos.html. A key with any other
// fingerprint is rejected.
const (
	aptKeyURL      = "https://zrepl.cschwarz.com/apt/apt-key.asc"
	aptKeyFPR      = "E101418FD3D6FBCB9D65A62D708699FC5F2EBF16"
	aptRepoBase    = "https://zrepl.cschwarz.com/apt/debian"
	aptKeyring     = "/usr/share/keyrings/zrepl.gpg"
	aptSourcesFile = "/etc/apt/sources.list.d/zrepl.list"
)

var versionPattern = regexp.MustCompile(`version=(v[0-9][^ ]*)`)

// Version returns the installed zrepl version, or "" if zrepl isn't
// installed.
func (a *Applier) Version(ctx context.Context) string {
	out, err := a.Run(ctx, "zrepl", "version")
	if err != nil {
		return ""
	}
	if m := versionPattern.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// Install adds zrepl's apt repository (verifying its signing key), installs
// zrepl, and holds the package: until zrepl 1.0 the repository publishes
// breaking releases immediately, so EZDR upgrades deliberately.
func (a *Applier) Install(ctx context.Context) error {
	codename, err := debianCodename()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "zrepl-key-*.asc")
	if err != nil {
		return err
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := a.Run(ctx, "curl", "-fsSL", "-o", tmp.Name(), aptKeyURL); err != nil {
		return fmt.Errorf("download zrepl signing key: %w", err)
	}
	out, err := a.Run(ctx, "gpg", "--show-keys", "--with-colons", tmp.Name())
	if err != nil {
		return fmt.Errorf("read zrepl signing key: %w", err)
	}
	if fpr := firstFingerprint(string(out)); fpr != aptKeyFPR {
		return fmt.Errorf("zrepl signing key has fingerprint %s, expected %s; not installing", fpr, aptKeyFPR)
	}
	if _, err := a.Run(ctx, "gpg", "--batch", "--yes", "--dearmor", "-o", aptKeyring, tmp.Name()); err != nil {
		return fmt.Errorf("install zrepl signing key: %w", err)
	}
	source := fmt.Sprintf("deb [arch=amd64 signed-by=%s] %s %s main\n", aptKeyring, aptRepoBase, codename)
	if err := writeFile(aptSourcesFile, []byte(source), 0o644); err != nil {
		return err
	}
	for _, s := range [][]string{
		{"apt-get", "update", "-qq"},
		{"apt-get", "install", "-y", "-qq", "zrepl"},
		{"apt-mark", "hold", "zrepl"},
	} {
		if _, err := a.Run(ctx, s[0], s[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(s, " "), err)
		}
	}
	return nil
}

func firstFingerprint(colons string) string {
	for _, line := range strings.Split(colons, "\n") {
		f := strings.Split(line, ":")
		if len(f) > 9 && f[0] == "fpr" {
			return f[9]
		}
	}
	return ""
}

func debianCodename() (string, error) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VERSION_CODENAME="); ok {
			return strings.Trim(v, `"`), nil
		}
	}
	return "", fmt.Errorf("no VERSION_CODENAME in /etc/os-release")
}
