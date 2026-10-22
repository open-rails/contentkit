package versions

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
)

// removeLegacyPreviews is an idempotent upgrade from copied public preview
// uploads. New plans sign existing renditions; chapter edits never call this.
func (s *Service) removeLegacyPreviews(ctx context.Context, k *media.Kind, t Target) error {
	if k.Publish.Previews == "" {
		return nil
	}
	spec := public(k, k.Publish.Previews)
	if spec == nil {
		return nil
	}
	m, err := s.manifest(ctx, k, t)
	if err != nil {
		return err
	}
	var ops []media.Op
	for _, f := range m.Files {
		if f.IsUpload() && inPattern(spec.From, f.Path) {
			ops = append(ops, media.Op{Op: media.OpRemove, Path: f.Path})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	_, err = s.m.Uploads.Commit(ctx, system, ref, uuid.NewString(), ops)
	return err
}
