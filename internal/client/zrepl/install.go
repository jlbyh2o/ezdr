package zrepl

import (
	"bufio"
	"context"
	"errors"
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
	// --show client reports the installed binary without contacting the
	// daemon, which may be restarting.
	out, err := a.Run(ctx, "zrepl", "version", "--show", "client")
	if err != nil {
		return ""
	}
	if m := versionPattern.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// SupportedVersion is the zrepl release series EZDR configures, as an apt
// version pattern.
const SupportedVersion = "0.7.*"

// aptInstall installs zrepl in the supported series, keeping a modified
// zrepl.yml (a dpkg conffile) rather than stopping at dpkg's prompt, and
// holds the package: until zrepl 1.0 the repository publishes breaking
// releases immediately, so EZDR upgrades deliberately. It first finishes any
// interrupted dpkg run, such as an earlier attempt cut short when the client
// restarted.
var aptInstall = [][]string{
	{"dpkg", "--force-confdef", "--force-confold", "--configure", "-a"},
	{"apt-get", "update", "-qq"},
	{"apt-get", "install", "-y", "-qq", "--allow-downgrades", "--allow-change-held-packages",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "zrepl=" + SupportedVersion},
	{"apt-mark", "hold", "zrepl"},
}

// Install adds zrepl's apt repository (verifying its signing key) and
// installs zrepl.
func (a *Applier) Install(ctx context.Context) error {
	if err := a.ensureRepository(ctx); err != nil {
		return err
	}
	for _, s := range aptInstall {
		if _, err := a.Run(ctx, s[0], s[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(s, " "), err)
		}
	}
	return nil
}

// Upgrade installs the supported zrepl series if an older release is
// installed, and restarts zrepl so the new daemon runs. It returns the
// version now installed. Repeating it is safe, including after an attempt
// cut short midway (an interrupted dpkg run is finished first, and zrepl is
// held and restarted even if the new release is already on disk).
func (a *Applier) Upgrade(ctx context.Context) (string, error) {
	if a.Version(ctx) == "" {
		return "", errors.New("zrepl isn't installed")
	}
	if _, err := a.Run(ctx, aptInstall[0][0], aptInstall[0][1:]...); err != nil {
		return "", fmt.Errorf("finish an interrupted package installation: %w", err)
	}
	if !Supported(a.Version(ctx)) {
		if err := a.Install(ctx); err != nil {
			return "", err
		}
	}
	if _, err := a.Run(ctx, "apt-mark", "hold", "zrepl"); err != nil {
		return "", fmt.Errorf("hold zrepl: %w", err)
	}
	if _, err := a.Run(ctx, "systemctl", "restart", "zrepl"); err != nil {
		return "", fmt.Errorf("restart zrepl: %w", err)
	}
	v := a.Version(ctx)
	if !Supported(v) {
		return v, fmt.Errorf("zrepl %s is installed after the upgrade, not 0.7", v)
	}
	return v, nil
}

// Supported reports whether a zrepl version (such as "v0.7.0") is in the
// supported series.
func Supported(version string) bool {
	return strings.HasPrefix(version, "v0.7.") || version == "v0.7"
}

// ensureRepository adds zrepl's apt repository after verifying its signing
// key's fingerprint.
func (a *Applier) ensureRepository(ctx context.Context) error {
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
	// Every key in the file would be trusted: only the expected one may be
	// there.
	if fprs := primaryFingerprints(string(out)); len(fprs) != 1 || fprs[0] != aptKeyFPR {
		return fmt.Errorf("zrepl signing key file has key(s) %v, expected only %s; not installing", fprs, aptKeyFPR)
	}
	if _, err := a.Run(ctx, "gpg", "--batch", "--yes", "--dearmor", "-o", aptKeyring, tmp.Name()); err != nil {
		return fmt.Errorf("install zrepl signing key: %w", err)
	}
	source := fmt.Sprintf("deb [arch=amd64 signed-by=%s] %s %s main\n", aptKeyring, aptRepoBase, codename)
	return writeFile(aptSourcesFile, []byte(source), 0o644)
}

// primaryFingerprints returns the fingerprints of the primary keys (not
// subkeys) in gpg's colon listing.
func primaryFingerprints(colons string) []string {
	var out []string
	primary := false
	for _, line := range strings.Split(colons, "\n") {
		f := strings.Split(line, ":")
		switch {
		case f[0] == "pub":
			primary = true
		case f[0] == "sub":
			primary = false
		case f[0] == "fpr" && primary && len(f) > 9:
			out = append(out, f[9])
			primary = false
		}
	}
	return out
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
