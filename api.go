package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (a *App) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	ip, ok := a.requireIP(w, r, true)
	if !ok {
		return
	}

	// Uploads per hour, keyed by token. Claimed before any bytes are read.
	count, err := bump(r.Context(), a.DB, fmt.Sprintf("uploads:%d", tokenID.Int64), 1, time.Hour)
	if err != nil {
		a.Log.Printf("rate limit: %v", err)
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return
	}
	if count > UploadsPerHour {
		writeError(w, http.StatusTooManyRequests, "Too many uploads from this token")
		return
	}

	// Hard ceiling on the request body, enforced by the runtime as it is read rather than
	// after the fact. The slack covers the multipart framing around the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, MaxBytes+1<<20)

	body, filename, expiresIn, err := readUploadPart(r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("The file is larger than %d MB", MaxBytes>>20))
			return
		}
		var rejected ErrRejected
		if errors.As(err, &rejected) {
			writeError(w, http.StatusUnprocessableEntity, rejected.Error())
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "The upload could not be read")
		return
	}

	// Daily byte cap. Claim first, check second, so two uploads racing on one token cannot
	// both read the same figure and both pass. A rejected upload hands its claim straight back.
	// The key is built once: recomputing the date at refund time would credit the next day's
	// counter for a claim made a second before UTC midnight.
	capKey := fmt.Sprintf("bytes:%d:%s", tokenID.Int64, time.Now().UTC().Format(time.DateOnly))
	claimed, err := bump(r.Context(), a.DB, capKey, int64(len(body)), 48*time.Hour)
	if err != nil {
		a.Log.Printf("daily cap: %v", err)
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return
	}
	refund := func() {
		if _, err := bump(r.Context(), a.DB, capKey, -int64(len(body)), 48*time.Hour); err != nil {
			a.Log.Printf("refunding daily cap: %v", err)
		}
	}
	if claimed > DailyByteCap {
		refund()
		writeError(w, http.StatusTooManyRequests, "Daily upload limit reached")
		return
	}

	upload, err := a.CreateUpload(r.Context(), body, filename, ip, tokenID, expiresIn)
	if err != nil {
		refund()
		var rejected ErrRejected
		if errors.As(err, &rejected) {
			writeError(w, http.StatusUnprocessableEntity, rejected.Error())
			return
		}
		if strings.HasPrefix(err.Error(), "storage:") {
			a.Log.Printf("storage failure: %v", err)
			writeError(w, http.StatusBadGateway, "Storage is unavailable, try again")
			return
		}
		a.Log.Printf("creating upload: %v", err)
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return
	}

	url := a.Store.URL(upload.ObjectKey)
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":          url,
		"markdown":     upload.Markdown(url),
		"content_type": upload.ContentType,
		"delete_url":   a.Config.BaseURL + "/api/v1/uploads/" + upload.DeleteToken,
		"expires_at":   expiresJSON(upload),
		"byte_size":    upload.ByteSize,
	})
}

// expiresJSON is the RFC 3339 expiry, or null for an upload kept until deleted.
func expiresJSON(u Upload) any {
	if !u.Expires() {
		return nil
	}
	return u.ExpiresAt.Format(time.RFC3339)
}

// readUploadPart walks the multipart body and returns the file, its declared name and any
// expires_in field. The file is read through a limit reader so a lying Content-Length cannot
// talk us into an unbounded allocation.
func readUploadPart(r *http.Request) (body []byte, filename, expiresIn string, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", "", err
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", "", err
		}
		switch part.FormName() {
		case "file":
			filename = part.FileName()
			body, err = readCapped(part, MaxBytes,
				fmt.Sprintf("The file is larger than %d MB", MaxBytes>>20))
		case "expires_in":
			var v []byte
			v, err = readCapped(part, 64, "expires_in is too long")
			expiresIn = string(v)
		default:
			_, err = io.Copy(io.Discard, io.LimitReader(part, 4<<10))
		}
		part.Close()
		if err != nil {
			return nil, "", "", err
		}
	}
	if len(body) == 0 {
		return nil, "", "", ErrRejected{"No file was sent"}
	}
	return body, filename, expiresIn, nil
}

func readCapped(part *multipart.Part, limit int64, tooBig string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrRejected{tooBig}
	}
	return b, nil
}

func (a *App) handleDeleteUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(w, r); !ok {
		return
	}
	upload, err := a.FindUploadBy(r.Context(), "delete_token", r.PathValue("deleteToken"))
	if err == sql.ErrNoRows {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		a.Log.Printf("looking up delete token: %v", err)
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return
	}
	if err := a.Purge(r.Context(), upload, "deleted", false); err != nil {
		a.Log.Printf("deleting %s: %v", upload.ObjectKey, err)
		writeError(w, http.StatusBadGateway, "Storage is unavailable, try again")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authenticate resolves the bearer token. Every upload is attributable to one, so abuse
// always has an owner to revoke.
func (a *App) authenticate(w http.ResponseWriter, r *http.Request) (sql.NullInt64, bool) {
	raw, err := BearerToken(r.Header.Get("Authorization"), r.URL.Query().Get("token"))
	if err == ErrTokenInQuery {
		writeError(w, http.StatusUnauthorized,
			"Send the token in the Authorization header. A token in the query string leaks into "+
				"access logs, browser history and Referer headers - treat this one as compromised "+
				"and create another.")
		return sql.NullInt64{}, false
	}
	tokenID, ok := a.Authenticate(r.Context(), raw)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing or invalid API token")
		return sql.NullInt64{}, false
	}
	return tokenID, true
}

// requireIP resolves the client address or refuses the request. The API answers in JSON,
// the pages in plain text; the message and the log line are the same either way.
func (a *App) requireIP(w http.ResponseWriter, r *http.Request, asJSON bool) (string, bool) {
	ip, ok := a.clientIP(r)
	if !ok {
		a.Log.Printf("refused a request with no usable client address from %s", r.RemoteAddr)
		const msg = "This request did not arrive through the trusted proxy"
		if asJSON {
			writeError(w, http.StatusForbidden, msg)
		} else {
			http.Error(w, msg, http.StatusForbidden)
		}
		return "", false
	}
	return ip, true
}
