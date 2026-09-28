// mwserver 启动维护窗口编排的 HTTP 服务。
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/chris64233/go-maintenance-window/maintenance"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "maintenance.json", "path to the JSON data file")
	flag.Parse()

	store, err := maintenance.OpenStore(*data)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	svc := maintenance.NewService(store)

	// 后台周期性地做超时处理，与手动完成/中止竞争时只产生一份定案。
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			svc.Expire()
		}
	}()

	log.Printf("maintenance window server listening on %s (data: %s)", *addr, *data)
	log.Fatal(http.ListenAndServe(*addr, maintenance.NewHandler(svc)))
}
