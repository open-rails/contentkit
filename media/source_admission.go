package media

import (
	"errors"
	"slices"
	"strings"
)

// SourceAdmission records the verified upload policy accepted for a ZIP source.
// Only server commit code writes it; HTTP operations cannot supply these fields.
// A background importer uses the same owner and ceiling when attaching decoded
// originals. Quota <= 0 is unlimited, as in PGLimits.Quota.
type SourceAdmission struct {
	// Generation is the accepting commit ID. Removing and re-adding a retained
	// ZIP creates fresh work; metadata edits keep the existing generation.
	Generation string `json:"generation"`
	Target     string `json:"target"`
	Accounting bool   `json:"accounting,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Enforce    bool   `json:"enforce,omitempty"`
	Quota      int64  `json:"quota,omitempty"`
}

// ArchiveDestination returns this upload's configured image destination. The reader
// preset's existing mapping remains supported; generic kinds can set ArchiveTo.
func (k *RegisteredKind) ArchiveDestination(source string) string {
	i, _, _, _, ok := k.upload(source)
	if !ok {
		return ""
	}
	if target := k.Uploads[i].ArchiveTo; target != "" {
		return target
	}
	if k.Publish != nil && k.Uploads[i].Path == k.Publish.Import {
		return k.Publish.Pages
	}
	return ""
}

func (o *opRun) admitSource(f *File, previous int) error {
	if o.k.ArchiveDestination(f.Path) == "" {
		return nil
	}
	if previous >= 0 && o.m.Files[previous].Source() == f.Source() && o.m.Files[previous].Admission != nil {
		prior := o.m.Files[previous].Admission
		f.Admission = prior
		if o.trusted || o.admission == nil {
			return nil
		}
		fresh, err := o.admission(f.Path, o.k.ArchiveDestination(f.Path))
		if err != nil {
			return err
		}
		if fresh.Owner == prior.Owner {
			if prior.Generation != "" {
				fresh.Generation = prior.Generation
			}
			f.Admission = fresh
		}
		return nil
	}
	if o.admission == nil {
		return errors.New("media: ZIP upload has no verified source admission")
	}
	admission, err := o.admission(f.Path, o.k.ArchiveDestination(f.Path))
	if err != nil {
		return err
	}
	if admission.Enforce && admission.Owner == "" {
		return uploadErr(CodeForbidden, "quota-enforced ZIP upload requires an owner")
	}
	f.Admission = admission
	return nil
}

func validateArchiveDestination(k *RegisteredKind, source Upload) error {
	if source.ArchiveTo == "" {
		return nil
	}
	if !slices.Equal(source.Types, []string{"application/zip"}) {
		return errors.New("media: archive_to source accepts only application/zip")
	}
	target := k.uploadIndex(source.ArchiveTo)
	if !strings.HasSuffix(source.ArchiveTo, "{name}") || target < 0 {
		return errors.New("media: archive_to must name a configured {name} image upload")
	}
	for _, typ := range k.Uploads[target].Types {
		if !strings.HasPrefix(typ, "image/") {
			return errors.New("media: archive_to destination accepts only images")
		}
	}
	return nil
}

func (k *RegisteredKind) pendingArchives(m *Manifest) bool {
	for _, f := range m.Files {
		if f.IsUpload() && f.Fail() == nil && k.ArchiveDestination(f.Path) != "" && (f.Blob == "" || f.Meta["archive_completed_sha256"] != sourceDigest(f)) {
			return true
		}
	}
	return false
}

// archiveBatch identifies only this source's admitted destination within the
// same manifest; a matching SHA in another target is a different batch.
func (k *RegisteredKind) archiveBatch(source File) (string, string) {
	if source.Type != "application/zip" {
		return "", ""
	}
	target := k.ArchiveDestination(source.Path)
	if source.Admission != nil && source.Admission.Target != "" {
		target = source.Admission.Target
	}
	return target, sourceDigest(source)
}
func (k *RegisteredKind) archiveChild(file File, target, digest string) bool {
	if !file.IsUpload() || digest == "" {
		return false
	}
	group, _, _, _, ok := k.upload(file.Path)
	if !ok || k.Uploads[group].Path != target {
		return false
	}
	meta, _ := pageMetadata(file.Meta)
	return meta["archive_sha256"] == digest
}
func (o *opRun) attachArchiveBatch(source File) error {
	target, digest := o.k.archiveBatch(source)
	if target == "" || digest == "" {
		return nil
	}
	if o.archiveAllowed == nil {
		return uploadErr(CodeForbidden, "archive attachment requires an approved destination")
	}
	if err := o.archiveAllowed(source.Path, target); err != nil {
		return err
	}
	paths := []string{}
	for _, file := range o.m.Files {
		if file.Unattached && o.k.archiveChild(file, target, digest) {
			paths = append(paths, file.Path)
		}
	}
	for _, path := range paths {
		if err := o.apply(-1, Op{Op: OpAttach, Path: path}); err != nil {
			return err
		}
	}
	return nil
}
func (o *opRun) cancelledArchivePages(source File) []string {
	if !source.Unattached {
		return nil
	}
	target, digest := o.k.archiveBatch(source)
	if target == "" || digest == "" {
		return nil
	}
	for _, other := range o.m.Files {
		otherTarget, otherDigest := o.k.archiveBatch(other)
		if other.Path != source.Path && other.IsUpload() && otherTarget == target && otherDigest == digest && o.k.ArchiveDestination(other.Path) != "" {
			return nil
		}
	}
	paths := []string{}
	for _, file := range o.m.Files {
		if file.Unattached && o.k.archiveChild(file, target, digest) {
			paths = append(paths, file.Path)
		}
	}
	return paths
}
