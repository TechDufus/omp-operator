/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package chart

import (
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	keyDashboard    = "dashboard"
	keyOIDC         = "oidc"
	keyURL          = "url"
	shortIDP        = "https://i"
	keyIssuerURL    = "issuerURL"
	keyClientID     = "clientID"
	keyClientSecret = "clientSecret"
	keyExisting     = "existingSecret"
	idpURL          = "https://idp.example.com"
	dashboardURL    = "https://omp.example.com/"
)

// accessFile mirrors the access file the chart renders for the dashboard.
type accessFile struct {
	OIDC struct {
		IssuerURL        string   `json:"issuerURL"`
		ClientID         string   `json:"clientID"`
		ClientSecretFile string   `json:"clientSecretFile"`
		RedirectURL      string   `json:"redirectURL"`
		Scopes           []string `json:"scopes"`
		UsernameClaim    string   `json:"usernameClaim"`
		GroupsClaim      string   `json:"groupsClaim"`
	} `json:"oidc"`
	Basic struct {
		HtpasswdFile string `json:"htpasswdFile"`
	} `json:"basic"`
	Rules []struct {
		Users      []string `json:"users"`
		Groups     []string `json:"groups"`
		Namespaces []string `json:"namespaces"`
		Role       string   `json:"role"`
	} `json:"rules"`
}

func dashboardValues(access []any) map[string]any {
	return map[string]any{
		keyDashboard: map[string]any{
			keyURL: dashboardURL,
			keyOIDC: map[string]any{
				keyIssuerURL:    idpURL,
				keyClientID:     "omp",
				keyClientSecret: map[string]any{keyExisting: "omp-oidc"},
			},
			"basic":  map[string]any{keyExisting: "omp-basic"},
			"access": access,
		},
		"hooks": map[string]any{"publicURL": "https://hooks.example.com"},
	}
}

func TestDashboardAccessConfigMap(t *testing.T) {
	access := []any{map[string]any{
		"users": []string{"alice@example.com", "basic:ops"}, "groups": []string{"platform"},
		"namespaces": []string{"*"}, "role": "viewer",
	}}
	r := render(t, dashboardValues(access))

	cm := r.configMaps["omp-operator-access"]
	if cm == nil {
		t.Fatal("access ConfigMap not rendered")
	}
	var got accessFile
	if err := yaml.UnmarshalStrict([]byte(cm.Data["access.yaml"]), &got); err != nil {
		t.Fatalf("access.yaml: %v\n%s", err, cm.Data["access.yaml"])
	}
	eq(t, "issuerURL", got.OIDC.IssuerURL, idpURL)
	eq(t, "clientID", got.OIDC.ClientID, "omp")
	eq(t, "clientSecretFile", got.OIDC.ClientSecretFile, "/etc/omp-operator/oidc/client-secret")
	eq(t, "redirectURL", got.OIDC.RedirectURL, "https://omp.example.com/auth/callback")
	eqList(t, "scopes", got.OIDC.Scopes, []string{"openid", "email", "profile", "groups"})
	eq(t, "usernameClaim", got.OIDC.UsernameClaim, "email")
	eq(t, "groupsClaim", got.OIDC.GroupsClaim, "groups")
	eq(t, "htpasswdFile", got.Basic.HtpasswdFile, "/etc/omp-operator/basic/htpasswd")
	eq(t, "rules", len(got.Rules), 1)
	eqList(t, "rule users", got.Rules[0].Users, []string{"alice@example.com", "basic:ops"})
	eqList(t, "rule groups", got.Rules[0].Groups, []string{"platform"})
	eqList(t, "rule namespaces", got.Rules[0].Namespaces, []string{"*"})
	eq(t, "rule role", got.Rules[0].Role, "viewer")
}

func TestDashboardChecksumAnnotation(t *testing.T) {
	access := []any{map[string]any{"users": []string{"alice"}, "namespaces": []string{"*"}, "role": "viewer"}}
	values := dashboardValues(access)
	sum := render(t, values).deployments[operatorName].Spec.Template.Annotations["checksum/access"]
	eq(t, "checksum length", len(sum), 64)

	access[0].(map[string]any)["namespaces"] = []string{"prod"}
	changed := render(t, values).deployments[operatorName].Spec.Template.Annotations["checksum/access"]
	yes(t, "checksum changes with the access rules", changed != sum && len(changed) == 64)
}

func TestDashboardMountsAndFlags(t *testing.T) {
	r := render(t, dashboardValues(nil))
	ps := r.deployments[operatorName].Spec.Template.Spec

	oidc := volumeByName(ps.Volumes, "oidc-client-secret")
	if oidc.Secret == nil || len(oidc.Secret.Items) != 1 {
		t.Fatalf("oidc volume: %+v", oidc)
	}
	eq(t, "oidc secret", oidc.Secret.SecretName, "omp-oidc")
	eq(t, "oidc key", oidc.Secret.Items[0].Key, "client-secret")
	eq(t, "oidc path", oidc.Secret.Items[0].Path, "client-secret")

	basic := volumeByName(ps.Volumes, "basic-htpasswd")
	if basic.Secret == nil || len(basic.Secret.Items) != 1 {
		t.Fatalf("basic volume: %+v", basic)
	}
	eq(t, "basic secret", basic.Secret.SecretName, "omp-basic")
	eq(t, "basic key", basic.Secret.Items[0].Key, "htpasswd")
	eq(t, "basic path", basic.Secret.Items[0].Path, "htpasswd")

	mounts := mountPaths(ps.Containers[0])
	eq(t, "oidc mount", mounts["/etc/omp-operator/oidc"], "oidc-client-secret")
	eq(t, "basic mount", mounts["/etc/omp-operator/basic"], "basic-htpasswd")

	args := managerArgs(t, r)
	yes(t, "--dashboard-url", slices.Contains(args, "--dashboard-url="+dashboardURL))
	yes(t, "--hooks-public-url", slices.Contains(args, "--hooks-public-url=https://hooks.example.com"))
}

func TestOIDCRequiresItsInputs(t *testing.T) {
	secret := map[string]any{keyExisting: "s"}
	cases := map[string]map[string]any{
		"no clientID": {keyURL: "https://x", keyOIDC: map[string]any{keyIssuerURL: shortIDP, keyClientSecret: secret}},
		"no url":      {keyOIDC: map[string]any{keyIssuerURL: shortIDP, keyClientID: "c", keyClientSecret: secret}},
		"no secret":   {keyURL: "https://x", keyOIDC: map[string]any{keyIssuerURL: shortIDP, keyClientID: "c"}},
	}
	for name, dashboard := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := helm(t, map[string]any{keyDashboard: dashboard}); err == nil {
				t.Fatal("helm template succeeded, want a required-value error")
			}
		})
	}
}
