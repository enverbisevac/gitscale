package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/enverbisevac/gitlib/git"
	"github.com/enverbisevac/gitlib/log"
)

// one or more key=value pairs separated by colons
var safeGitProtocolHeader = regexp.MustCompile(`^[0-9a-zA-Z]+=[0-9a-zA-Z]+(:[0-9a-zA-Z]+=[0-9a-zA-Z]+)*$`)

type serviceConfig struct {
	UploadPack  bool
	ReceivePack bool
	Env         []string
}

type serviceHandler struct {
	cfg     *serviceConfig
	w       http.ResponseWriter
	r       *http.Request
	body    io.Reader
	dir     string
	environ []string
}

func (h *serviceHandler) setHeaderNoCache() {
	h.w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.w.Header().Set("Pragma", "no-cache")
	h.w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

func prepareGitCmdWithAllowedService(service string, h *serviceHandler) (*git.Command, error) {
	if service == "receive-pack" && h.cfg.ReceivePack {
		return git.NewCommand(h.r.Context(), "receive-pack"), nil
	}
	if service == "upload-pack" && h.cfg.UploadPack {
		return git.NewCommand(h.r.Context(), "upload-pack"), nil
	}

	return nil, fmt.Errorf("service %q is not allowed", service)
}

func serviceRPC(h *serviceHandler, service string, resultChan chan<- result) {
	syncNode(h.dir)
	expectedContentType := fmt.Sprintf("application/x-git-%s-request", service)
	if h.r.Header.Get("Content-Type") != expectedContentType {
		log.Error("Content-Type (%q) doesn't match expected: %q", h.r.Header.Get("Content-Type"), expectedContentType)
		h.w.WriteHeader(http.StatusUnauthorized)
		return
	}

	cmd, err := prepareGitCmdWithAllowedService(service, h)
	if err != nil {
		log.Error("Failed to prepareGitCmdWithService: %v", err)
		h.w.WriteHeader(http.StatusUnauthorized)
		return
	}

	reqBody := h.body

	// Handle GZIP.
	if h.r.Header.Get("Content-Encoding") == "gzip" {
		reqBody, err = gzip.NewReader(reqBody)
		if err != nil {
			log.Error("Fail to create gzip reader: %v", err)
			h.w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	// set this for allow pre-receive and post-receive execute
	h.environ = append(h.environ, "SSH_ORIGINAL_COMMAND="+service)

	if protocol := h.r.Header.Get("Git-Protocol"); protocol != "" && safeGitProtocolHeader.MatchString(protocol) {
		h.environ = append(h.environ, "GIT_PROTOCOL="+protocol)
	}

	var stderr bytes.Buffer
	cmd.AddArguments("--stateless-rpc").AddDynamicArguments(h.dir)
	cmd.SetDescription(fmt.Sprintf("%s %s %s [repo_path: %s]", git.GitExecutable, service, "--stateless-rpc", h.dir))
	err = cmd.Run(&git.RunOpts{
		Dir:               h.dir,
		Env:               append(os.Environ(), h.environ...),
		Stdout:            h.w,
		Stdin:             reqBody,
		Stderr:            &stderr,
		UseContextTimeout: true,
	})
	if err != nil {
		if err.Error() != "signal: killed" {
			log.Error("Fail to serve RPC(%s) in %s: %v - %s", service, h.dir, err, stderr.String())
		}
		resultChan <- result{
			node: h.dir,
			err:  err,
		}
		return
	}
	if resultChan != nil {
		resultChan <- result{
			node: h.dir,
		}
	}
}

func getServiceType(r *http.Request) string {
	serviceType := r.FormValue("service")
	if !strings.HasPrefix(serviceType, "git-") {
		return ""
	}
	return strings.TrimPrefix(serviceType, "git-")
}

func packetWrite(str string) []byte {
	s := strconv.FormatInt(int64(len(str)+4), 16)
	if len(s)%4 != 0 {
		s = strings.Repeat("0", 4-len(s)%4) + s
	}
	return []byte(s + str)
}

func syncNode(failedNode string) {
	filename := path.Join(failedNode, "outofsync")
	file, err := os.Open(filename)
	if err != nil {
		log.Warn("outofsync file %s not found, err: %v", filename, err)
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		log.Warn("content %s of outofsync file cannot be read, err: %v", filename, err)
		return
	}
	valid := strings.TrimSpace(string(data))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = git.NewCommand(ctx, "remote", "add", "sync", git.CmdArgCheck(valid)).Run(&git.RunOpts{
		Dir: failedNode,
	})
	if err != nil {
		log.Warn("error setting remote for %s, err: %v", failedNode, err)
		return
	}
	err = git.NewCommand(ctx, "fetch", "sync").Run(&git.RunOpts{
		Dir: failedNode,
	})
	if err != nil {
		log.Warn("failed to fetch from %s, err: %v", valid, err)
		return
	}
	err = git.NewCommand(ctx, "remote", "rm", "sync").Run(&git.RunOpts{
		Dir: failedNode,
	})
	if err != nil {
		log.Warn("failed to remove remote sync for %s, err: %v", failedNode, err)
		return
	}
	err = os.Remove(filename)
	if err != nil {
		log.Warn("failed to remove outofsync for %s, err: %v", failedNode, err)
		return
	}
}
