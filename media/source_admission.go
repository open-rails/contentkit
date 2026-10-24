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
	Owner   string `json:"owner,omitempty"`
	Enforce bool   `json:"enforce,omitempty"`
	Quota   int64  `json:"quota,omitempty"`
}

// archiveTo returns this upload's configured image destination. The reader
// preset's existing mapping remains supported; generic kinds can set ArchiveTo.
func (k *Kind) archiveTo(source string) string {
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
	if o.k.archiveTo(f.Path) == "" {
		return nil
	}
	if previous >= 0 && o.m.Files[previous].Source() == f.Source() && o.m.Files[previous].Admission != nil {
		prior := o.m.Files[previous].Admission
		f.Admission = prior
		if o.trusted || o.admission == nil {
			return nil
		}
		fresh, err := o.admission()
		if err != nil {
			return err
		}
		if fresh.Owner == prior.Owner {
			f.Admission = fresh
		}
		return nil
	}
	if o.admission == nil {
		return errors.New("media: ZIP upload has no verified source admission")
	}
	admission, err := o.admission()
	if err != nil {
		return err
	}
	if admission.Enforce && admission.Owner == "" {
		return uploadErr(CodeForbidden, "quota-enforced ZIP upload requires an owner")
	}
	f.Admission = admission
	return nil
}

func validateArchiveDestination(k *Kind, source Upload) error {
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
