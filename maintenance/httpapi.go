package maintenance

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// NewHandler 返回暴露编排服务的 HTTP 接口：
//
//	POST /resources                 登记资源        {"id","name"}
//	GET  /resources                 资源列表
//	POST /requests                  创建申请        {"resource_ids","start","duration_minutes","prep_steps"}
//	GET  /requests/{id}             查询申请
//	POST /requests/{id}/modify      修改申请（递增版本）
//	POST /requests/{id}/prep        准备回执        {"version","step"}
//	POST /requests/{id}/start       开始维护
//	POST /requests/{id}/complete    完成            {"reason"}
//	POST /requests/{id}/abort       中止            {"reason"}
//	POST /expire                    触发超时处理
//	GET  /calendar?from=&to=        日历查询（RFC3339）
//	GET  /history                   定案历史
func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /resources", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		res, err := svc.RegisterResource(in.ID, in.Name)
		respond(w, res, err)
	})
	mux.HandleFunc("GET /resources", func(w http.ResponseWriter, r *http.Request) {
		respond(w, svc.Resources(), nil)
	})

	mux.HandleFunc("POST /requests", func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeRequestInput(w, r)
		if !ok {
			return
		}
		req, err := svc.CreateRequest(in)
		respond(w, req, err)
	})
	mux.HandleFunc("GET /requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		req, err := svc.GetRequest(r.PathValue("id"))
		respond(w, req, err)
	})
	mux.HandleFunc("POST /requests/{id}/modify", func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeRequestInput(w, r)
		if !ok {
			return
		}
		req, err := svc.ModifyRequest(r.PathValue("id"), in)
		respond(w, req, err)
	})
	mux.HandleFunc("POST /requests/{id}/prep", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int    `json:"version"`
			Step    string `json:"step"`
		}
		if !decode(w, r, &in) {
			return
		}
		respond(w, map[string]string{"status": "ok"}, svc.ReportPrep(r.PathValue("id"), in.Version, in.Step))
	})
	mux.HandleFunc("POST /requests/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]string{"status": "ok"}, svc.Start(r.PathValue("id")))
	})
	mux.HandleFunc("POST /requests/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]string{"status": "ok"}, svc.Complete(r.PathValue("id"), reasonBody(w, r)))
	})
	mux.HandleFunc("POST /requests/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]string{"status": "ok"}, svc.Abort(r.PathValue("id"), reasonBody(w, r)))
	})
	mux.HandleFunc("POST /expire", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string][]string{"expired": svc.Expire()}, nil)
	})
	mux.HandleFunc("GET /calendar", func(w http.ResponseWriter, r *http.Request) {
		from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
		to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
		if err1 != nil || err2 != nil {
			respond(w, nil, errors.New("from/to must be RFC3339 timestamps"))
			return
		}
		respond(w, svc.Calendar(from, to), nil)
	})
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		respond(w, svc.History(), nil)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respond(w, nil, err)
		return false
	}
	return true
}

func decodeRequestInput(w http.ResponseWriter, r *http.Request) (RequestInput, bool) {
	var in struct {
		ResourceIDs     []string `json:"resource_ids"`
		Start           string   `json:"start"`
		DurationMinutes int      `json:"duration_minutes"`
		PrepSteps       []string `json:"prep_steps"`
	}
	if !decode(w, r, &in) {
		return RequestInput{}, false
	}
	start, err := time.Parse(time.RFC3339, in.Start)
	if err != nil {
		respond(w, nil, errors.New("start must be an RFC3339 timestamp"))
		return RequestInput{}, false
	}
	return RequestInput{
		ResourceIDs: in.ResourceIDs,
		Start:       start,
		Duration:    time.Duration(in.DurationMinutes) * time.Minute,
		PrepSteps:   in.PrepSteps,
	}, true
}

func reasonBody(w http.ResponseWriter, r *http.Request) string {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in) // reason 可缺省
	return in.Reason
}

func respond(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
