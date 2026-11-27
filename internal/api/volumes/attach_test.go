package volumes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

func TestValidateRequests(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	nine := make([]CreateVolumeRequest, MaxVolumesPerSandbox+1)
	for index := range nine {
		nine[index] = CreateVolumeRequest{Name: "v" + string(rune('a'+index)), MountPath: "/mnt/" + string(rune('a'+index))}
	}
	for _, test := range []struct {
		name     string
		requests []CreateVolumeRequest
		valid    bool
	}{
		{"by name and by digest", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a"}, {Digest: digest, MountPath: "/mnt/b", Optional: true}}, true},
		{"none", nil, true},
		{"more than the drives", nine, false},
		{"name and digest", []CreateVolumeRequest{{Name: "a", Digest: digest, MountPath: "/mnt/a"}}, false},
		{"neither", []CreateVolumeRequest{{MountPath: "/mnt/a"}}, false},
		{"invalid name", []CreateVolumeRequest{{Name: "A", MountPath: "/mnt/a"}}, false},
		{"digest without its prefix", []CreateVolumeRequest{{Digest: strings.Repeat("a", 64), MountPath: "/mnt/a"}}, false},
		{"mount path outside /mnt", []CreateVolumeRequest{{Name: "a", MountPath: "/home/user"}}, false},
		{"one name twice", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a"}, {Name: "a", MountPath: "/mnt/b"}}, false},
		{"one path below another", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a"}, {Name: "b", MountPath: "/mnt/a/b"}}, false},
	} {
		var requestErr *RequestError
		if err := ValidateRequests(test.requests); (err == nil) != test.valid || (err != nil && !errors.As(err, &requestErr)) {
			t.Errorf("%s: ValidateRequests() = %v, want valid %t", test.name, err, test.valid)
		}
	}
}

func TestMatches(t *testing.T) {
	have := []SandboxVolume{
		{Name: "a", Digest: "sha256:1", MountPath: "/mnt/a", Attached: true},
		{Digest: "sha256:2", MountPath: "/mnt/b", Optional: true, Reason: LeftOutNotFound},
	}
	for _, test := range []struct {
		name     string
		requests []CreateVolumeRequest
		want     bool
	}{
		{"same, other order", []CreateVolumeRequest{{Digest: "sha256:2", MountPath: "/mnt/b", Optional: true}, {Name: "a", MountPath: "/mnt/a"}}, true},
		{"the first by its digest", []CreateVolumeRequest{{Digest: "sha256:1", MountPath: "/mnt/a"}, {Digest: "sha256:2", MountPath: "/mnt/b", Optional: true}}, true},
		{"other mount path", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/c"}, {Digest: "sha256:2", MountPath: "/mnt/b", Optional: true}}, false},
		{"other optional", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a", Optional: true}, {Digest: "sha256:2", MountPath: "/mnt/b", Optional: true}}, false},
		{"one less", []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a"}}, false},
	} {
		if got := Matches(have, test.requests); got != test.want {
			t.Errorf("%s: Matches() = %t, want %t", test.name, got, test.want)
		}
	}
}

func TestPrepareFindsVolumesByNameAndDigest(t *testing.T) {
	env := newTestEnv(t)
	named, unnamed, deleted := validTar(t), makeTar(t, file("b", "b")), makeTar(t, file("c", "c"))
	for _, published := range []struct {
		data []byte
		name string
	}{{named, "named"}, {unnamed, ""}, {deleted, "deleted"}} {
		if _, err := env.publish(t, published.data, published.name); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.service.Delete(context.Background(), digestOf(deleted), RoleAdmin); err != nil {
		t.Fatal(err)
	}
	attacher := NewAttacher(env.records, env.objects, volume.KindTar)

	attachment, err := attacher.Prepare(context.Background(), []CreateVolumeRequest{
		{Name: "named", MountPath: "/mnt/a"},
		{Digest: "sha256:" + digestOf(unnamed), MountPath: "/mnt/b", Optional: true},
		{Name: "deleted", MountPath: "/mnt/c", Optional: true},
		{Digest: "sha256:" + digestOf(deleted), MountPath: "/mnt/d", Optional: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SandboxVolume{
		{Name: "named", Digest: "sha256:" + digestOf(named), Scope: volume.SharedScope, MountPath: "/mnt/a"},
		{Digest: "sha256:" + digestOf(unnamed), Scope: volume.SharedScope, MountPath: "/mnt/b", Optional: true},
		{Name: "deleted", MountPath: "/mnt/c", Optional: true, Reason: LeftOutNotFound},
		{Digest: "sha256:" + digestOf(deleted), MountPath: "/mnt/d", Optional: true, Reason: LeftOutNotFound},
	}
	if !slices.Equal(attachment.Volumes, want) {
		t.Errorf("volumes = %+v, want %+v", attachment.Volumes, want)
	}
	runner := attachment.RunnerVolumes()
	if len(runner) != 2 || runner[0].SHA256 != digestOf(named) || runner[1].SHA256 != digestOf(unnamed) {
		t.Fatalf("runner volumes = %v, want the two found", runner)
	}
	if runner[0].Kind != volume.KindTar || runner[0].Size != int64(len(named)) || runner[0].Tar.Entries == 0 ||
		!strings.HasPrefix(runner[0].URL, "https://api.test:8443/") || runner[0].MountPath != "/mnt/a" || runner[0].Optional {
		t.Errorf("runner volume 0 = %+v (url %q), want the tar of named with its counts and a read URL", runner[0], runner[0].URL)
	}

	_, err = attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "deleted", MountPath: "/mnt/a"}})
	if requiredErr := (*RequiredError)(nil); !errors.As(err, &requiredErr) || requiredErr.Index != 0 || requiredErr.Reason != LeftOutNotFound {
		t.Errorf("Prepare() of a required volume not found = %v, want RequiredError not_found", err)
	}
	_, err = attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "named", MountPath: "/mnt/a"}, {Digest: "sha256:" + digestOf(named), MountPath: "/mnt/b"}})
	if requestErr := (*RequestError)(nil); !errors.As(err, &requestErr) {
		t.Errorf("Prepare() of one volume by name and by digest = %v, want RequestError", err)
	}
}

func TestPrepareOnFirecrackerSendsImages(t *testing.T) {
	env := newTestEnv(t)
	withoutImage := validTar(t)
	if _, err := env.publish(t, withoutImage, "old"); err != nil {
		t.Fatal(err)
	}
	env.withBuilds(newFakeBuilds(testImage()).build, builder("runner-a", 0))
	if _, err := env.publish(t, makeTar(t, file("b", "b")), "new"); err != nil {
		t.Fatal(err)
	}
	attacher := NewAttacher(env.records, env.objects, volume.KindExt4)

	attachment, err := attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "new", MountPath: "/mnt/a"}, {Name: "old", MountPath: "/mnt/b", Optional: true}})
	if err != nil {
		t.Fatal(err)
	}
	runner := attachment.RunnerVolumes()
	image := testImage()
	if len(runner) != 1 || runner[0].Kind != volume.KindExt4 || runner[0].SHA256 != digestOf(image) || runner[0].Size != int64(len(image)) || runner[0].Tar != (volume.TarCounts{}) {
		t.Fatalf("runner volumes = %v, want the image of new only", runner)
	}
	if got := attachment.Volumes[1].Reason; got != LeftOutNoImage {
		t.Errorf("reason of the volume without an image = %q, want no_image", got)
	}
	_, err = attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "old", MountPath: "/mnt/b"}})
	if requiredErr := (*RequiredError)(nil); !errors.As(err, &requiredErr) || requiredErr.Reason != LeftOutNoImage {
		t.Errorf("Prepare() of a required volume without an image = %v, want RequiredError no_image", err)
	}
}

// readyImage stores a ready shared volume row with an image of imageBytes, and
// no objects.
func readyImage(t *testing.T, env *testEnv, name string, imageBytes int64) {
	t.Helper()
	row := &store.SharedVolume{ID: name, SHA256: digestOf([]byte(name)), Name: name, PublishedBy: string(RoleAdmin)}
	if _, err := env.records.StartSharedVolumePublish(context.Background(), row, time.Minute, 100); err != nil {
		t.Fatal(err)
	}
	row.TarBytes, row.TarEntries, row.ImageBytes, row.ImageSHA256 = 1, 1, imageBytes, digestOf([]byte("image of "+name))
	if ok, err := env.records.FinishSharedVolumePublish(context.Background(), row); err != nil || !ok {
		t.Fatalf("finish = %t, %v", ok, err)
	}
}

func TestPrepareHoldsVolumesToTheCap(t *testing.T) {
	env := newTestEnv(t)
	const sixGiB = 6 << 30 // 6 GiB
	readyImage(t, env, "big-a", sixGiB)
	readyImage(t, env, "big-b", sixGiB)
	readyImage(t, env, "small", 1<<30) // 1 GiB
	attacher := NewAttacher(env.records, env.objects, volume.KindExt4)

	// The optional volume first in the request still comes after the
	// required one.
	attachment, err := attacher.Prepare(context.Background(), []CreateVolumeRequest{
		{Name: "big-a", MountPath: "/mnt/a", Optional: true},
		{Name: "big-b", MountPath: "/mnt/b"},
		{Name: "small", MountPath: "/mnt/c", Optional: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := attachment.Volumes[0].Reason; got != LeftOutTooLarge {
		t.Errorf("reason of the optional volume over the cap = %q, want too_large", got)
	}
	if runner := attachment.RunnerVolumes(); len(runner) != 2 || runner[0].MountPath != "/mnt/b" || runner[1].MountPath != "/mnt/c" {
		t.Errorf("runner volumes = %v, want the required one and the small one", runner)
	}

	_, err = attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "big-a", MountPath: "/mnt/a"}, {Name: "big-b", MountPath: "/mnt/b"}})
	if requiredErr := (*RequiredError)(nil); !errors.As(err, &requiredErr) || requiredErr.Index != 1 || requiredErr.Reason != LeftOutTooLarge {
		t.Errorf("Prepare() of required volumes over the cap = %v, want RequiredError too_large at 1", err)
	}
}

// noReadURLs is a volume store that issues no read URL.
type noReadURLs struct{ volumestore.Store }

func (noReadURLs) ReadURL(context.Context, volumestore.Key, time.Duration) (string, error) {
	return "", errors.New("store helper down")
}

func TestPrepareWithoutReadURLs(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.publish(t, validTar(t), "a"); err != nil {
		t.Fatal(err)
	}
	attacher := NewAttacher(env.records, noReadURLs{env.objects}, volume.KindTar)

	attachment, err := attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a", Optional: true}})
	if err != nil || attachment.Volumes[0].Reason != LeftOutStoreUnavailable || len(attachment.RunnerVolumes()) != 0 {
		t.Fatalf("Prepare() of an optional volume = %+v, %v; want it left out with store_unavailable", attachment, err)
	}
	_, err = attacher.Prepare(context.Background(), []CreateVolumeRequest{{Name: "a", MountPath: "/mnt/a"}})
	if requiredErr := (*RequiredError)(nil); !errors.As(err, &requiredErr) || requiredErr.Reason != LeftOutStoreUnavailable {
		t.Errorf("Prepare() of a required volume = %v, want RequiredError store_unavailable", err)
	}
}

func TestApplyReadsTheEcho(t *testing.T) {
	env := newTestEnv(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := env.publish(t, makeTar(t, file(name, name)), name); err != nil {
			t.Fatal(err)
		}
	}
	prepare := func() *PendingAttachment {
		attachment, err := NewAttacher(env.records, env.objects, volume.KindTar).Prepare(context.Background(), []CreateVolumeRequest{
			{Name: "a", MountPath: "/mnt/a"}, {Name: "b", MountPath: "/mnt/b", Optional: true}, {Name: "c", MountPath: "/mnt/c", Optional: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return attachment
	}
	echoOf := func(attachment *PendingAttachment) []runnerruntime.AppliedVolume {
		options := runnerruntime.CreateOptions{Volumes: attachment.RunnerVolumes()}
		return options.Applied(map[int]string{1: runnerruntime.VolumeLeftOutTimeout, 2: "new_reason"}).Volumes
	}

	attachment := prepare()
	if err := attachment.Apply(echoOf(attachment)); err != nil {
		t.Fatal(err)
	}
	if got := attachment.Volumes; !got[0].Attached || got[1].Attached || got[1].Reason != runnerruntime.VolumeLeftOutTimeout || got[2].Reason != LeftOutError {
		t.Errorf("volumes = %+v, want a attached, b left out for timeout, c for error", got)
	}

	for name, change := range map[string]func(echo []runnerruntime.AppliedVolume) []runnerruntime.AppliedVolume{
		"none echoed": func([]runnerruntime.AppliedVolume) []runnerruntime.AppliedVolume { return nil },
		"another sha256": func(echo []runnerruntime.AppliedVolume) []runnerruntime.AppliedVolume {
			echo[1].SHA256 = strings.Repeat("0", 64)
			return echo
		},
		"the required one left out": func(echo []runnerruntime.AppliedVolume) []runnerruntime.AppliedVolume {
			echo[0].Attached, echo[0].Reason = false, runnerruntime.VolumeLeftOutMount
			return echo
		},
	} {
		attachment := prepare()
		before := slices.Clone(attachment.Volumes)
		if err := attachment.Apply(change(echoOf(attachment))); !errors.Is(err, ErrVolumesMismatch) || !slices.Equal(attachment.Volumes, before) {
			t.Errorf("%s: Apply() = %v with volumes %+v, want ErrVolumesMismatch and nothing recorded", name, err, attachment.Volumes)
		}
	}
}
