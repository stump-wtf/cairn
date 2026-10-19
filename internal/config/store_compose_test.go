package config

// Guards which object store the shipped stack names, which is the thing issue
// #388 got wrong: the guide and docker-compose.yml each pointed at a MinIO image,
// both registries stopped serving them anonymously, and every documented way to
// stand Cairn up failed at `docker compose pull`. CI had been running Garage the
// whole time, so "S3-compatible" was a claim the pipeline tested and the
// documentation assumed.
//
// The invariant is therefore about agreement, not about any one vendor: the image
// the guide tells people to pull, the image `make up` pulls, and the image the
// integration job stands up are one image at one tag. When a registry stops
// serving it, all three move together, and a drift between the documented path
// and the tested one is a failing test rather than a broken self-hosting story.
//
// @joestump-agent 10/19/2026 - Added for cairn#388.

import (
	"os"
	"regexp"
	"testing"
)

var (
	garageImageRe = regexp.MustCompile(`dxflrs/garage:([A-Za-z0-9._-]+)`)
	minioImageRe  = regexp.MustCompile(`(?m)^\s*image:\s*(\S*minio\S*)`)
)

// imageTag returns the single Garage tag a file names, failing the test when it
// names none (a store swapped out silently) or more than one (a file that pins
// two versions of itself).
func imageTag(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tag := ""
	seen := map[string]bool{}
	for _, m := range garageImageRe.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
		tag = m[1]
	}
	if len(seen) == 0 {
		t.Errorf("%s names no Garage image: the store it starts is untested by this repo", path)
		return ""
	}
	if len(seen) > 1 {
		t.Errorf("%s pins more than one Garage tag: %v", path, keys(seen))
	}
	return tag
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestShippedObjectStoreMatchesTheTestedOne(t *testing.T) {
	ci := imageTag(t, "../../.gitea/workflows/pipeline.yaml")
	dev := imageTag(t, "../../docker-compose.yml")
	guide := imageTag(t, "../../website/docs/guides/self-hosting.md")
	if ci == "" || dev == "" || guide == "" {
		return
	}
	if dev != ci {
		t.Errorf("docker-compose.yml pins Garage %s but CI tests %s: the store developers run is not the store cairn's S3 code is exercised against", dev, ci)
	}
	if guide != ci {
		t.Errorf("the self-hosting guide pins Garage %s but CI tests %s: a fresh instance runs an image nothing tests", guide, ci)
	}
}

// The MinIO images are what broke: quay.io answers 401 and the Docker Hub
// repositories are gone, so naming one in either documented path makes that path
// unrunnable no matter what the store is nominally compatible with.
func TestShippedStackNamesNoUnpullableMinioImage(t *testing.T) {
	for _, path := range []string{"../../docker-compose.yml", "../../website/docs/guides/self-hosting.md"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range minioImageRe.FindAllStringSubmatch(string(src), -1) {
			t.Errorf("%s pulls %s: MinIO's registries no longer serve anonymous pulls (cairn#388), so this path fails at `docker compose pull`", path, m[1])
		}
	}
}

// The guide's compose block has to point cairn at the store that block starts.
func TestGuideComposePointsAtTheStoreItStarts(t *testing.T) {
	vars := guideComposeVars(t)
	endpoint, ok := vars["CAIRN_S3_ENDPOINT"]
	if !ok {
		t.Fatal("the guide's compose file sets no CAIRN_S3_ENDPOINT")
	}
	if endpoint != "garage:3900" {
		t.Errorf("CAIRN_S3_ENDPOINT = %q: the garage service in the same block serves S3 on garage:3900", endpoint)
	}
}
