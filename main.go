package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"

	"github.com/enverbisevac/gitlib/git"
	"github.com/enverbisevac/gitlib/log"
)

const (
	node1 = "/Users/enver/Projects/gitscale/repos/node1.git"
	node2 = "/Users/enver/Projects/gitscale/repos/node2.git"
	node3 = "/Users/enver/Projects/gitscale/repos/node3.git"
)

var (
	nodes = []string{node1, node2, node3}
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", statusHandler)

	mux.HandleFunc("/info/refs", infoRefsHandler)
	mux.HandleFunc("/git-upload-pack", uploadPackHandler)
	mux.HandleFunc("/git-receive-pack", receivePackHandler)

	server := http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	err := server.ListenAndServe()
	if err != nil {
		log.Fatal("error running server: %v", err)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "Status")
	w.WriteHeader(http.StatusOK)
}

func infoRefsHandler(w http.ResponseWriter, r *http.Request) {
	syncNode(node3)
	h := &serviceHandler{
		cfg: &serviceConfig{
			UploadPack:  true,
			ReceivePack: true,
		},
		w:   w,
		r:   r,
		dir: node1,
	}
	h.setHeaderNoCache()
	service := getServiceType(h.r)
	cmd, err := prepareGitCmdWithAllowedService(service, h)
	if err == nil {
		if protocol := h.r.Header.Get("Git-Protocol"); protocol != "" && safeGitProtocolHeader.MatchString(protocol) {
			h.environ = append(h.environ, "GIT_PROTOCOL="+protocol)
		}
		h.environ = append(os.Environ(), h.environ...)

		refs, _, err := cmd.AddArguments("--stateless-rpc", "--advertise-refs", ".").RunStdBytes(&git.RunOpts{Env: h.environ, Dir: h.dir})
		if err != nil {
			log.Error(fmt.Sprintf("%v - %s", err, string(refs)))
		}

		h.w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-advertisement", service))
		h.w.WriteHeader(http.StatusOK)
		_, _ = h.w.Write(packetWrite("# service=git-" + service + "\n"))
		_, _ = h.w.Write([]byte("0000"))
		_, _ = h.w.Write(refs)
	}
}

func uploadPackHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := r.Body.Close(); err != nil {
			log.Error("uploadPackHandler: Close: %v", err)
		}
	}()
	h := serviceHandler{
		cfg: &serviceConfig{
			UploadPack: true,
		},
		w:    w,
		r:    r,
		body: r.Body,
		dir:  node2,
	}
	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", "upload-pack"))
	serviceRPC(&h, "upload-pack", nil)
}

type result struct {
	node string
	err  error
}

func receivePackHandler(w http.ResponseWriter, r *http.Request) {
	wg := sync.WaitGroup{}
	readers := make([]*io.PipeReader, len(nodes))
	writers := make([]io.Writer, len(nodes))
	gitResult := make(chan result, 100)

	defer func() {
		if err := r.Body.Close(); err != nil {
			log.Error("receivePackHandler: Close: %v", err)
		}
	}()

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", "receive-pack"))

	for i := range nodes {
		pr, pw := io.Pipe()
		readers[i] = pr
		writers[i] = pw
	}
	writer := AsyncMultiWriter(writers...)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(writer, r.Body)

		for i := range nodes {
			pw := writers[i].(*io.PipeWriter)
			pw.Close()
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusExpectationFailed)
			return
		}
	}()

	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := serviceHandler{
				cfg: &serviceConfig{
					ReceivePack: true,
				},
				w:    w,
				r:    r,
				body: readers[i],
				dir:  nodes[i],
			}
			serviceRPC(&h, "receive-pack", gitResult)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		slice := make([]result, 0, len(nodes))
		failed := make([]string, 0, 8)
		healthy := ""
	loop:
		for {
			select {
			case <-r.Context().Done():
				return
			case res := <-gitResult:
				if res.err != nil {
					lockFile(res.node, healthy)
					failed = append(failed, res.node)
				}

				if healthy == "" {
					healthy = res.node
				}

				slice = append(slice, res)
				if len(slice) == len(nodes) {
					break loop
				}
			}
		}
		for _, val := range failed {
			lockFile(val, healthy)
		}
	}()

	wg.Wait()
}

func lockFile(node, healthy string) {
	filename := path.Join(node, "outofsync")
	file, err := os.OpenFile(filename, os.O_CREATE, 0o644)
	if err != nil {
		log.Error("Error creating outofsync file, err: %v", err)
		return
	}
	defer file.Close()

	io.WriteString(file, healthy)
}
