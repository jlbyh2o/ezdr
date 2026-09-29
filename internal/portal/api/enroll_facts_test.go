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
