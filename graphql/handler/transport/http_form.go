package transport

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// MultipartForm the Multipart request spec https://github.com/jaydenseric/graphql-multipart-request-spec
type MultipartForm struct {
	// MaxUploadSize sets the maximum number of bytes used to parse a request body
	// as multipart/form-data.
	MaxUploadSize int64
}

var _ graphql.Transport = MultipartForm{}

func (f MultipartForm) Supports(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}

	return r.Method == "POST" && mediaType == "multipart/form-data"
}

func (f MultipartForm) maxUploadSize() int64 {
	if f.MaxUploadSize == 0 {
		return 32 << 20
	}
	return f.MaxUploadSize
}

func (f MultipartForm) Do(w http.ResponseWriter, r *http.Request, exec graphql.GraphExecutor) {
	w.Header().Set("Content-Type", "application/json")

	start := graphql.Now()

	var err error
	if r.ContentLength > f.maxUploadSize() {
		writeJsonError(w, "failed to parse multipart form, request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, f.maxUploadSize())
	defer r.Body.Close()

	mr, err := r.MultipartReader()
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonError(w, "failed to parse multipart form")
		return
	}

	part, err := mr.NextPart()
	if err != nil || part.FormName() != "operations" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonError(w, "first part must be operations")
		return
	}

	var params graphql.RawParams
	if err = jsonDecode(part, &params); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonError(w, "operations form field could not be decoded")
		return
	}

	part, err = mr.NextPart()
	if err != nil || part.FormName() != "map" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonError(w, "second part must be map")
		return
	}

	uploadsMap := map[string][]string{}
	if err = json.NewDecoder(part).Decode(&uploadsMap); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonError(w, "map form field could not be decoded")
		return
	}

	for {
		part, err = mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeJsonErrorf(w, "failed to parse part")
			return
		}

		key := part.FormName()
		filename := part.FileName()
		contentType := part.Header.Get("Content-Type")

		paths := uploadsMap[key]
		if len(paths) == 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeJsonErrorf(w, "invalid empty operations paths list for key %s", key)
			return
		}
		delete(uploadsMap, key)

		var (
			upload graphql.Upload
			err    *gqlerror.Error
		)
		for _, path := range paths {
			upload = graphql.Upload{
				File:        part,
				Size:        r.ContentLength,
				Filename:    filename,
				ContentType: contentType,
			}

			if err = params.AddUpload(upload, key, path); err != nil {
				w.WriteHeader(http.StatusUnprocessableEntity)
				writeJsonGraphqlError(w, err)
				return
			}
		}
	}

	for key := range uploadsMap {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJsonErrorf(w, "failed to get key %s from form", key)
		return
	}

	params.Headers = r.Header

	params.ReadTime = graphql.TraceTiming{
		Start: start,
		End:   graphql.Now(),
	}

	rc, gerr := exec.CreateOperationContext(r.Context(), &params)
	if gerr != nil {
		resp := exec.DispatchError(graphql.WithOperationContext(r.Context(), rc), gerr)
		w.WriteHeader(statusFor(gerr))
		writeJson(w, resp)
		return
	}
	responses, ctx := exec.DispatchOperation(r.Context(), rc)
	writeJson(w, responses(ctx))
}
