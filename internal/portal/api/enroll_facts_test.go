package api

import (
	"testing"

	enrollv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1"
)

func TestValidateFactsHostname(t *testing.T) {
	for name, ok := range map[string]bool{
		"pve1": true, "pve-dr.example.com": true, "PVE01": true,
		"": false, "-pve": false, "pve\nargs: -x": false, "pve 1": false, "pve_1": false, "pve.": false,
	} {
		err := validateFacts(&enrollv1.HostFacts{Hostname: name, MachineId: "m"})
		if (err == nil) != ok {
			t.Errorf("hostname %q: %v", name, err)
		}
	}
}

func TestInstallScriptURL(t *testing.T) {
	for v, want := range map[string]string{
		"0.1.0":                  "https://github.com/jlbyh2o/ezdr/releases/download/v0.1.0/install.sh",
		"dev":                    "https://github.com/jlbyh2o/ezdr/releases/latest/download/install.sh",
		"0.0.0-SNAPSHOT-d4e8a01": "https://github.com/jlbyh2o/ezdr/releases/latest/download/install.sh",
	} {
		if got := InstallScriptURL(v); got != want {
			t.Errorf("InstallScriptURL(%q) = %s", v, got)
		}
	}
}
