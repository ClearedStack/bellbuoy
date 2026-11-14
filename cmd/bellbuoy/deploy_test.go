package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ClearedStack/bellbuoy/internal/ownership"
)

var pinnedImageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:+-]*@sha256:[0-9a-f]{64}$`)

// TestDeployManifestsAndPosture checks that deploy/ and .ko.yaml meet the
// cluster deployment rules without needing a cluster or third-party YAML
// library.
func TestDeployManifestsAndPosture(t *testing.T) {
	koRaw, err := os.ReadFile("../../.ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	koText := string(koRaw)
	var baseImage string
	for _, line := range strings.Split(koText, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "defaultBaseImage:"); ok {
			baseImage = strings.TrimSpace(rest)
		}
	}
	if !pinnedImageRE.MatchString(baseImage) {
		t.Fatalf(".ko.yaml defaultBaseImage = %q, want image pinned by @sha256:<64 hex>", baseImage)
	}
	if !strings.Contains(koText, "- linux/arm64") {
		t.Fatalf(".ko.yaml does not target linux/arm64:\n%s", koText)
	}

	appRaw, err := os.ReadFile("../../deploy/bellbuoy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	netRaw, err := os.ReadFile("../../deploy/networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	appText := string(appRaw)
	netText := string(netRaw)

	for name, text := range map[string]string{
		"deploy/bellbuoy.yaml":      appText,
		"deploy/networkpolicy.yaml": netText,
	} {
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "\t") {
				t.Errorf("%s line %d contains a tab", name, i+1)
			}
		}
		if strings.Contains(text, "kind: Secret") {
			t.Errorf("%s defines a Secret in Git", name)
		}
		for _, doc := range strings.Split(text, "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			if !strings.Contains(doc, "\n  namespace: cs-workers\n") {
				t.Errorf("%s has a manifest outside namespace cs-workers:\n%s", name, doc)
			}
		}
	}

	// Image in Deployment must be pinned by sha256 digest in Lazarette.
	var deployImages []string
	for _, line := range strings.Split(appText, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "image:"); ok {
			deployImages = append(deployImages, strings.TrimSpace(rest))
		}
	}
	if len(deployImages) != 1 || !pinnedImageRE.MatchString(deployImages[0]) ||
		!strings.HasPrefix(deployImages[0], "lazarette.clearedstack.svc.cluster.local:5000/bellbuoy@sha256:") {
		t.Fatalf("Deployment images = %v, want single Lazarette image pinned by @sha256", deployImages)
	}

	// Pod posture, affinity, resource budget, and mounts.
	requiredInApp := []string{
		"kind: ServiceAccount",
		"kind: ConfigMap",
		"kind: Deployment",
		"serviceAccountName: bellbuoy",
		"key: kubernetes.io/arch\n                    operator: In\n                    values: [\"arm64\"]",
		"key: kubernetes.io/hostname\n                    operator: NotIn\n                    values: [\"cm3588-plus\"]",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"type: RuntimeDefault",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"drop: [\"ALL\"]",
		"cpu: 10m",
		"memory: 16Mi",
		"memory: 30Mi",
		"mountPath: /var/spool/cs",
		"mountPath: /etc/bellbuoy\n              readOnly: true",
		"optional: true",
	}
	for _, needle := range requiredInApp {
		if !strings.Contains(appText, needle) {
			t.Errorf("deploy/bellbuoy.yaml missing %q", needle)
		}
	}

	// Extract and parse the embedded ownership_map.json using Bellbuoy's own parser.
	mapJSON := extractEmbeddedOwnershipMap(t, appText)
	owners, err := ownership.Parse([]byte(mapJSON))
	if err != nil {
		t.Fatalf("embedded ownership_map.json failed ownership.Parse: %v", err)
	}
	for _, repo := range []string{
		"clearedstack.com", "muster", "logbook", "plainforge", "lazarette", "keelworks", "shipkeeper",
		"trawl", "sextant", "assay", "countersign", "waybill", "moldloft", "shakedown", "chartroom", "tidegauge", "bellbuoy",
	} {
		if !owners.Known(repo) {
			t.Errorf("embedded ownership_map.json does not list repo %s", repo)
		}
	}
	if elem, ok := owners.ElementForTarget("worker.bellbuoy.manifest"); !ok || elem.Repository != "bellbuoy" || elem.Kind != "CONFIG" {
		t.Errorf("worker.bellbuoy.manifest = %+v, %v", elem, ok)
	}

	// NetworkPolicy: default-deny plus narrow egress only, never Logbook.
	requiredInNet := []string{
		"name: default-deny",
		"podSelector: {}",
		"- Ingress\n    - Egress",
		"name: bellbuoy-egress",
		"ingress: []",
		"kubernetes.io/metadata.name: kube-system",
		"app.kubernetes.io/name: muster",
		"app.kubernetes.io/name: plainforge",
		"cidr: 0.0.0.0/0",
		"- 10.0.0.0/8",
		"- 172.16.0.0/12",
		"- 192.168.0.0/16",
		"- 100.64.0.0/10",
		"- 127.0.0.0/8",
		"- 169.254.0.0/16",
		"port: 443",
	}
	for _, needle := range requiredInNet {
		if !strings.Contains(netText, needle) {
			t.Errorf("deploy/networkpolicy.yaml missing %q", needle)
		}
	}
	if strings.Contains(netText, "logbook") || strings.Contains(netText, "5432") {
		t.Errorf("deploy/networkpolicy.yaml must never open access to Logbook")
	}
}

func extractEmbeddedOwnershipMap(t *testing.T, yamlText string) string {
	t.Helper()
	const marker = "  ownership_map.json: |\n"
	idx := strings.Index(yamlText, marker)
	if idx < 0 {
		t.Fatal("ownership_map.json block not found in deploy/bellbuoy.yaml")
	}
	rest := yamlText[idx+len(marker):]
	var lines []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(line, "    ") {
			lines = append(lines, strings.TrimPrefix(line, "    "))
			continue
		}
		if strings.TrimSpace(line) == "" {
			lines = append(lines, "")
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}

// TestRepositoryContainsNoEmDashes enforces the platform rule against U+2014
// across source, tests, fixtures, manifests and documentation.
func TestRepositoryContainsNoEmDashes(t *testing.T) {
	emDash := []byte("\xe2\x80\x94")
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, emDash) {
			t.Errorf("%s contains an em dash (U+2014)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
