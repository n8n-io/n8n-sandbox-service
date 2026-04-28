package store

import (
	"errors"
	"testing"
)

func TestStorePersistsDockerMetadata(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	rec := &SandboxRecord{
		ID:             "sandbox-1",
		Status:         "running",
		CreatedAt:      1,
		LastActiveAt:   2,
		ContainerID:    "container-1",
		ContainerIP:    "172.30.0.2",
		DaemonPort:     8081,
		ImageID:        "img-1",
		NetworkPolicy:  `{"allowed_ips":["8.8.8.8/32"]}`,
		ResourceLimits: `{"memory_mb":512}`,
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}

	got, err := s.Get(rec.ID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if got == nil {
		t.Fatal("expected record")
	}
	if got.ContainerID != rec.ContainerID || got.ContainerIP != rec.ContainerIP || got.DaemonPort != rec.DaemonPort || got.ImageID != rec.ImageID {
		t.Fatalf("unexpected docker metadata: %+v", got)
	}
}

func TestStoreImageCRUD(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	image := &ImageRecord{
		ID:              "img-1",
		Tag:             "sandbox-custom-1",
		BaseImage:       "sandbox-base:latest",
		DockerImageID:   "sha256:abc",
		StepsHash:       "hash-1",
		DockerfileSteps: `["RUN echo hello"]`,
		CreatedAt:       10,
	}
	if err := s.CreateImage(image); err != nil {
		t.Fatalf("create image: %v", err)
	}

	gotByID, err := s.GetImage(image.ID)
	if err != nil {
		t.Fatalf("get image by id: %v", err)
	}
	if gotByID == nil || gotByID.Tag != image.Tag {
		t.Fatalf("unexpected image by id: %+v", gotByID)
	}

	gotByTag, err := s.GetImage(image.Tag)
	if err != nil {
		t.Fatalf("get image by tag: %v", err)
	}
	if gotByTag == nil || gotByTag.ID != image.ID {
		t.Fatalf("unexpected image by tag: %+v", gotByTag)
	}

	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(images) != 1 || images[0].ID != image.ID {
		t.Fatalf("unexpected images: %+v", images)
	}

	if err := s.Create(&SandboxRecord{
		ID:           "sandbox-1",
		Status:       "running",
		CreatedAt:    1,
		LastActiveAt: 1,
		ImageID:      image.ID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	count, err := s.CountSandboxesByImageID(image.ID)
	if err != nil {
		t.Fatalf("count sandboxes by image id: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 sandbox using image, got %d", count)
	}

	if err := s.DeleteImage(image.ID); err != nil {
		t.Fatalf("delete image: %v", err)
	}
	gotAfterDelete, err := s.GetImage(image.ID)
	if err != nil {
		t.Fatalf("get image after delete: %v", err)
	}
	if gotAfterDelete != nil {
		t.Fatalf("expected image to be deleted, got %+v", gotAfterDelete)
	}
}

func TestStoreRejectsDuplicateImageStepsHash(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	first := &ImageRecord{
		ID:              "img-1",
		Tag:             "sandbox-custom-1",
		BaseImage:       "sandbox-base:latest",
		DockerImageID:   "sha256:abc",
		StepsHash:       "hash-1",
		DockerfileSteps: `["RUN echo hello"]`,
		CreatedAt:       10,
	}
	if err := s.CreateImage(first); err != nil {
		t.Fatalf("create first image: %v", err)
	}

	second := &ImageRecord{
		ID:              "img-2",
		Tag:             "sandbox-custom-2",
		BaseImage:       "sandbox-base:latest",
		DockerImageID:   "sha256:def",
		StepsHash:       "hash-1",
		DockerfileSteps: `["RUN echo hello"]`,
		CreatedAt:       11,
	}
	err = s.CreateImage(second)
	if !errors.Is(err, ErrImageStepsHashConflict) {
		t.Fatalf("expected ErrImageStepsHashConflict, got %v", err)
	}
}
