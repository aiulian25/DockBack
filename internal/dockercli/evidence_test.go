package dockercli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Evidence keeps how a container is put together and drops its secrets:
// environment values and logging-option values.
func TestRedactInspectKeepsShapeDropsSecrets(t *testing.T) {
	raw := []byte(`{"Name":"/vaultwarden","Config":{"Image":"vaultwarden/server:1.32","Env":["ADMIN_TOKEN=s3cret","DOMAIN=https://vault.example"],"Labels":{"com.docker.compose.project":"vault"}},
		"HostConfig":{"Binds":["/srv/vault:/data"],"LogConfig":{"Type":"splunk","Config":{"splunk-token":"t0ken","splunk-url":"https://splunk"}}}}`)
	out, err := redactInspect(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, secret := range []string{"s3cret", "https://vault.example", "t0ken", "https://splunk"} {
		if strings.Contains(text, secret) {
			t.Errorf("%q must not survive redaction: %s", secret, text)
		}
	}
	var doc struct {
		Config struct {
			Image  string            `json:"Image"`
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			Binds     []string `json:"Binds"`
			LogConfig struct {
				Type   string            `json:"Type"`
				Config map[string]string `json:"Config"`
			} `json:"LogConfig"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Join(doc.Config.Env, ",") != "ADMIN_TOKEN,DOMAIN" {
		t.Errorf("environment names must be kept, in order: %v", doc.Config.Env)
	}
	if doc.Config.Image != "vaultwarden/server:1.32" || doc.Config.Labels["com.docker.compose.project"] != "vault" || doc.HostConfig.Binds[0] != "/srv/vault:/data" {
		t.Errorf("everything that is not a secret must be kept: %s", text)
	}
	if doc.HostConfig.LogConfig.Type != "splunk" || doc.HostConfig.LogConfig.Config["splunk-token"] != redactedValue {
		t.Errorf("logging keeps its driver and option keys, not their values: %+v", doc.HostConfig.LogConfig)
	}
}
