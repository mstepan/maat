package agent

import (
	"strings"
	"testing"
)

const validConfig = `{"id":"a","cluster_id":"test","bootstrap_seed":"a","initial_primary":"a","data_dir":"/var/lib/maat/data","state_dir":"/var/lib/maat/control","password_file":"/run/secrets/postgres","replication_password_file":"/run/secrets/replication","docker_socket":"/var/run/docker.sock","nodes":[{"id":"a","raft_address":"a:7000","http_address":"a:8000","postgres_address":"a:5432"},{"id":"b","raft_address":"b:7000","http_address":"b:8000","postgres_address":"b:5432"},{"id":"c","raft_address":"c:7000","http_address":"c:8000","postgres_address":"c:5432"}]}`

func TestConfigRejectsUnsafeInputs(t *testing.T) {
	for name, input := range map[string]string{
		"unknown":      strings.Replace(validConfig, `"id":"a"`, `"unexpected":true,"id":"a"`, 1),
		"duplicate":    strings.Replace(validConfig, `"id":"b"`, `"id":"a"`, 1),
		"relative":     strings.Replace(validConfig, `/var/lib/maat/data`, `data`, 1),
		"root":         strings.Replace(validConfig, `/var/lib/maat/data`, `/`, 1),
		"overlap":      strings.Replace(validConfig, `/var/lib/maat/data`, `/var/lib/maat/control/data`, 1),
		"negative":     strings.Replace(validConfig, `"id":"a"`, `"max_promotion_lag_bytes":-1,"id":"a"`, 1),
		"invalid_port": strings.Replace(validConfig, `a:7000`, `a:0`, 1),
		"trailing":     validConfig + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadConfig(strings.NewReader(input)); err == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
}
func TestConfigDefaultsAndZeroLag(t *testing.T) {
	c, err := ReadConfig(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPromotionLagBytes != 16777216 || c.MaxObservationAgeSeconds != 30 || c.FailureThreshold != 3 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	c, err = ReadConfig(strings.NewReader(strings.Replace(validConfig, `"id":"a"`, `"max_promotion_lag_bytes":0,"id":"a"`, 1)))
	if err != nil || c.MaxPromotionLagBytes != 0 {
		t.Fatalf("zero lag not preserved: %v", err)
	}
}

func TestConfigInstanceNodeIDs(t *testing.T) {
	input := strings.NewReplacer(`"a"`, `"instance-a"`, `"b"`, `"instance-b"`, `"c"`, `"instance-c"`, `a:`, `instance-a:`, `b:`, `instance-b:`, `c:`, `instance-c:`).Replace(validConfig)
	c, err := ReadConfig(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "instance-a" || c.InitialPrimary != "instance-a" || c.BootstrapSeed != "instance-a" {
		t.Fatalf("unexpected instance IDs: %+v", c)
	}
	if _, err := ReadConfig(strings.NewReader(strings.Replace(input, `"cluster_id":"test"`, `"cluster_id":"invalid-cluster"`, 1))); err == nil {
		t.Fatal("accepted a hyphenated cluster ID")
	}
}
