package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/media"
)

// A media kind no content or entity kind owns holds site images, such as the
// cover of a record the host keeps: staff upload it, anyone reads its public
// files, nobody else writes it.
func TestSiteMediaKind(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	id := uuid.Must(uuid.NewV7()).String()

	body := pngBytes(t, 32)
	sum := sha256.Sum256(body)
	presign := media.PresignBody{Ref: media.RefBody{Kind: "request", ID: id}, Path: "cover", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	f.want(http.StatusUnauthorized, "", "POST", "/media/upload/presign", presign, nil)
	f.want(http.StatusForbidden, user, "POST", "/media/upload/presign", presign, nil)
	var plan media.PresignReply
	f.want(http.StatusOK, staff, "POST", "/media/upload/presign", presign, &plan)
	req, _ := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, b)
	}
	commit := media.CommitBody{Ref: presign.Ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}
	f.want(http.StatusForbidden, user, "POST", "/media/upload/commit", commit, nil)
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", commit, nil)
	ref, err := w.reg.Ref("request", id)
	if err != nil {
		t.Fatal(err)
	}
	cover := publish(t, w, ref, plan.Path, "cover")

	var read media.ReadResult
	f.want(http.StatusOK, "", "GET", "/media/request/"+id, nil, &read)
	if len(read.Public) != 1 || read.Public[0].Preset != "cover" || len(read.Public[0].Renditions) != 1 || read.Public[0].Renditions[0].URL != cover.Renditions[0].URL {
		t.Fatalf("anyone reads the cover: %+v, published %+v", read, cover)
	}
	var mine media.ReadResult
	f.want(http.StatusOK, staff, "GET", "/media/request/"+id+"?editor", nil, &mine)
	if len(mine.Uploads) == 0 {
		t.Fatalf("staff edit it: %+v", mine)
	}
	var theirs media.ReadResult
	f.want(http.StatusOK, user, "GET", "/media/request/"+id+"?editor", nil, &theirs)
	if len(theirs.Uploads) != 0 || len(theirs.Public) != 1 {
		t.Fatalf("a reader sees the cover, not the uploads: %+v", theirs)
	}
}
