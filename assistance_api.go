package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

func (s *apiServer) hAssistance(w http.ResponseWriter, r *http.Request, id int64, action string) {
	method := http.MethodPost
	if action == "profile-changes" {
		method = http.MethodGet
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeErr(w, 405, "请使用 "+method)
		return
	}
	if id <= 0 {
		writeErr(w, 400, "无效的联系人ID")
		return
	}
	if action == "profile-changes" {
		out, err := GetProfileChanges(s.db, id)
		if err != nil {
			assistError(w, err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var req struct {
		Text  string `json:"text"`
		Style string `json:"style"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var extra interface{}
	if dec.Decode(&extra) != io.EOF {
		writeErr(w, 400, "请求只能包含一个JSON对象")
		return
	}
	if err := validateAssist(req.Text, req.Style, action == "rewrite"); err != nil {
		assistError(w, err)
		return
	}
	key := bearerToken(r)
	if key == "" {
		key = clientIP(r)
	}
	if s.ingestRL != nil {
		if ok, retry := s.ingestRL.Allow(key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, 429, "请求过于频繁，请稍后重试")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), ingestAnalyzeTimeout)
	defer cancel()
	if action == "rewrite" {
		reply, err := RewriteReply(ctx, s.db, s.llm, id, req.Text, req.Style)
		if err != nil {
			assistError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"reply": reply})
		return
	}
	out, err := ReviewDraft(ctx, s.db, s.llm, id, req.Text)
	if err != nil {
		assistError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func assistError(w http.ResponseWriter, err error) {
	code := 500
	if errors.Is(err, ErrAssistInput) {
		code = 400
	}
	if errors.Is(err, sql.ErrNoRows) {
		code = 404
	}
	writeErr(w, code, err.Error())
}
