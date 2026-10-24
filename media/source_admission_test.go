package media

import (
	"errors"
	"testing"
)

func TestArchiveAdmissionCannotComeFromClientMetadata(t *testing.T) {
	kind := admissionTestKind(t)
	file := File{Path: "zip/book.zip", Blob: "blob", Meta: map[string]any{"admission": map[string]any{"owner": "victim", "quota": 999999}}}
	runner := &opRun{k: kind, m: &Manifest{}}
	if err := runner.admitSource(&file, -1); err == nil {
		t.Fatal("client metadata admitted background quota authority")
	}
	runner.admission = func(string, string) (*SourceAdmission, error) {
		return &SourceAdmission{Owner: "actual", Enforce: true, Quota: 100}, nil
	}
	if err := runner.admitSource(&file, -1); err != nil {
		t.Fatal(err)
	}
	if file.Admission == nil || file.Admission.Owner != "actual" || file.Admission.Quota != 100 {
		t.Fatalf("admission=%+v", file.Admission)
	}
	runner.admission = func(string, string) (*SourceAdmission, error) { return nil, errors.New("policy unavailable") }
	file.Admission = nil
	if err := runner.admitSource(&file, -1); err == nil {
		t.Fatal("missing quota policy granted an exemption")
	}
}

func TestArchiveDestinationIsConfiguredWithoutCatalog(t *testing.T) {
	k := admissionTestKind(t)
	if got := k.ArchiveDestination("zip/book.zip"); got != "body/{name}" {
		t.Fatalf("generic archive destination=%q", got)
	}
	if err := validateArchiveDestination(k, k.Uploads[0]); err != nil {
		t.Fatal(err)
	}
	k.Uploads[0].ArchiveTo = "missing/{name}"
	if err := validateArchiveDestination(k, k.Uploads[0]); err == nil {
		t.Fatal("missing destination accepted")
	}
}

func admissionTestKind(t *testing.T) *Kind {
	t.Helper()
	reg, err := NewRegistry(Config{Namespace: "site", Kinds: []Kind{{Name: "post", Uploads: []Upload{{Path: "zip/{name}", ArchiveTo: "body/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 20}, {Path: "body/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Named: true, Max: 100}}}}})
	if err != nil {
		t.Fatal(err)
	}
	kind, err := reg.Kind("post")
	if err != nil {
		t.Fatal(err)
	}
	return kind
}
