package seal

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestImage is a large test (ADR 0031, ADR 0048): it needs the container
// engine and the toolchain image. It builds an image on the toolchain from a
// tar on standard input under a version's tag, with no network for its RUN,
// which sees the loopback interface only; then it tags the image to its plain
// name and removes the tag, as the new core's promotion does. Its cleanup
// leaves no image of its name on the engine.
//
// It runs only with KILN_ENGINE_TESTS=1, and then fails, not skips, when the
// engine cannot be reached.
func TestImage(t *testing.T) {
	if os.Getenv("KILN_ENGINE_TESTS") != "1" {
		t.Skip("needs the container engine; set KILN_ENGINE_TESTS=1")
	}
	if _, err := docker(t.Context(), nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("the container engine is not reachable: %v", err)
	}
	failIfStopped(t)
	name := "kiln-sealtest-" + randomID(t) + "-image"
	tag := "0123456789ab"
	tagged := name + ":" + tag
	t.Cleanup(func() { removeImagesOf(t, name) })
	src := imageTar(t, map[string]string{
		"Dockerfile": "ARG TOOLCHAIN\nFROM ${TOOLCHAIN}\nRUN [ \"$(grep -c : /proc/net/dev)\" = 1 ]\n",
	})
	err := BuildImage(t.Context(), Image{Name: name, Tag: tag, Dockerfile: "Dockerfile", Toolchain: toolchainImage, NoNetwork: true},
		bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := ImageID(t.Context(), name); err != nil || id != "" {
		t.Errorf("the plain name after the build: %q, %v", id, err)
	}
	built := must(t, "image", "inspect", "--format", "{{.Id}}", tagged)
	if id, err := ImageID(t.Context(), tagged); err != nil || id != built {
		t.Errorf("ImageID(%s) = %q, %v, want %q", tagged, id, err, built)
	}
	if err := TagImage(t.Context(), tagged, name); err != nil {
		t.Fatal(err)
	}
	if err := RemoveImageTag(t.Context(), tagged); err != nil {
		t.Fatal(err)
	}
	if id, err := ImageID(t.Context(), name); err != nil || id != built {
		t.Errorf("the plain name after the promotion: %q, %v, want %q", id, err, built)
	}
	if id, err := ImageID(t.Context(), tagged); err != nil || id != "" {
		t.Errorf("the tag after its removal: %q, %v", id, err)
	}
	// The promotion repeated after a kill tags again.
	if err := TagImage(t.Context(), name+":latest", name); err == nil {
		t.Error("a tag from latest was accepted")
	}
	if err := TagImage(t.Context(), tagged, name); err == nil {
		t.Error("a tag from a removed tag was accepted")
	}
	must(t, "tag", name, tagged)
	if err := TagImage(t.Context(), tagged, name); err != nil {
		t.Errorf("the tag repeated: %v", err)
	}
	if err := RemoveImageTag(t.Context(), tagged); err != nil {
		t.Fatal(err)
	}
	if tags, err := ImageTags(t.Context(), name); err != nil || !slices.Equal(tags, []string{"latest"}) {
		t.Errorf("ImageTags(%s) = %q, %v", name, tags, err)
	}
	if d, err := ImageDigest(t.Context(), name); err != nil || d != "" {
		t.Errorf("ImageDigest of an image without the label = %q, %v", d, err)
	}

	// A repo's own image: the plain name, with the label kiln.digest.
	plain := name + "-plain"
	t.Cleanup(func() { removeImagesOf(t, plain) })
	if d, err := ImageDigest(t.Context(), plain); err != nil || d != "" {
		t.Errorf("ImageDigest of a name with no image = %q, %v, want none", d, err)
	}
	src = imageTar(t, map[string]string{"Dockerfile": "ARG TOOLCHAIN\nFROM ${TOOLCHAIN}\n"})
	err = BuildImage(t.Context(), Image{Name: plain, Dockerfile: "Dockerfile", Toolchain: toolchainImage,
		Labels: map[string]string{DigestLabel: "d1gest"}}, bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if tags, err := ImageTags(t.Context(), plain); err != nil || !slices.Equal(tags, []string{"latest"}) {
		t.Errorf("ImageTags(%s) = %q, %v, want latest only", plain, tags, err)
	}
	if d, err := ImageDigest(t.Context(), plain); err != nil || d != "d1gest" {
		t.Errorf("ImageDigest(%s) = %q, %v, want d1gest", plain, d, err)
	}
}

// TestImageNames refuses names, tags and Dockerfiles that are not Kiln's
// before anything runs.
func TestImageNames(t *testing.T) {
	ctx := t.Context()
	good := Image{Name: "kiln-proxy", Tag: "0123456789ab", Dockerfile: "images/proxy/Dockerfile"}
	for _, im := range []Image{
		{Name: "-kiln", Tag: good.Tag, Dockerfile: good.Dockerfile},
		{Name: "Kiln", Tag: good.Tag, Dockerfile: good.Dockerfile},
		{Name: "example.com/kiln", Tag: good.Tag, Dockerfile: good.Dockerfile},
		{Name: good.Name, Tag: "latest", Dockerfile: good.Dockerfile},
		{Name: good.Name, Tag: "0123456789abc", Dockerfile: good.Dockerfile},
		{Name: good.Name, Tag: good.Tag, Dockerfile: "-f"},
		{Name: good.Name, Tag: good.Tag, Dockerfile: "/etc/Dockerfile"},
		{Name: good.Name, Tag: good.Tag, Dockerfile: "../Dockerfile"},
		{Name: good.Name, Tag: good.Tag, Dockerfile: good.Dockerfile, Toolchain: "--network=host"},
		{Name: good.Name, Tag: good.Tag, Dockerfile: good.Dockerfile, Toolchain: "kiln-toolchain:latest"},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{"Kiln.digest": "d"}},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{"--network": "host"}},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{"": "d"}},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{"kiln digest": "d"}},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{DigestLabel: "a\nb"}},
		{Name: good.Name, Dockerfile: good.Dockerfile, Labels: map[string]string{DigestLabel: "a\x7fb"}},
	} {
		if err := BuildImage(ctx, im, strings.NewReader("")); err == nil {
			t.Errorf("BuildImage(%+v): %v", im, err)
		}
	}
	for _, ref := range []string{"kiln-proxy:latest", "kiln-proxy:mine", "-kiln-proxy", "kiln-proxy:0123456789ab:x"} {
		if _, err := ImageID(ctx, ref); err == nil {
			t.Errorf("ImageID(%q) was accepted", ref)
		}
		if _, err := ImageDigest(ctx, ref); err == nil {
			t.Errorf("ImageDigest(%q) was accepted", ref)
		}
	}
	for _, ref := range []string{"kiln-proxy", "kiln-proxy:latest", "kiln-proxy:mine"} {
		if err := RemoveImageTag(ctx, ref); err == nil {
			t.Errorf("RemoveImageTag(%q) was accepted", ref)
		}
	}
	for _, c := range [][2]string{{"kiln-proxy", "kiln-proxy"}, {"kiln-proxy:0123456789ab", "kiln-proxy:mine"}, {"kiln-proxy:0123456789ab", "-x"}} {
		if err := TagImage(ctx, c[0], c[1]); err == nil {
			t.Errorf("TagImage(%q, %q) was accepted", c[0], c[1])
		}
	}
	if _, err := ImageTags(ctx, "kiln-proxy:latest"); err == nil {
		t.Error("ImageTags of a reference was accepted")
	}
}

// removeImagesOf removes every tagged reference of a test's image name, by
// reference, never by ID and never with --force, and fails the test with what
// is left of the name afterwards.
func removeImagesOf(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	refs, err := docker(ctx, nil, "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", name)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	for _, ref := range strings.Fields(refs) {
		if strings.HasSuffix(ref, ":<none>") {
			continue
		}
		if _, err := docker(ctx, nil, "image", "rm", ref); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
	left, err := docker(ctx, nil, "image", "ls", "--quiet", name)
	if err != nil {
		t.Errorf("cleanup: %v", err)
	} else if left != "" {
		refs, _ := docker(ctx, nil, "image", "ls", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}", name)
		t.Errorf("cleanup: the engine still has images of %s:\n%s", name, refs)
	}
}

// imageTar is a tar of files, as a context on standard input.
func imageTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, text := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
