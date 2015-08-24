/*
The gitHandler type implements http.Handler.

All code for handling Git HTTP requests is in this file.
*/

package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
)

type gitHandler struct {
	httpClient  *http.Client
	repoRoot    string
	authBackend string
}

type gitService struct {
	method     string
	suffix     string
	handleFunc func(gitEnv, string, string, http.ResponseWriter, *http.Request)
	rpc        string
}

type gitEnv struct {
	GL_ID string
}

// Routing table
var gitServices = [...]gitService{
	gitService{"GET", "/info/refs", handleGetInfoRefs, ""},
	gitService{"POST", "/git-upload-pack", handlePostRPC, "git-upload-pack"},
	gitService{"POST", "/git-receive-pack", handlePostRPC, "git-receive-pack"},
}

func newGitHandler(repoRoot, authBackend string) *gitHandler {
	return &gitHandler{&http.Client{}, repoRoot, authBackend}
}

func (h *gitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var env gitEnv
	var g gitService

	log.Printf("%s %q", r.Method, r.URL)

	// Look for a matching Git service
	foundService := false
	for _, g = range gitServices {
		if r.Method == g.method && strings.HasSuffix(r.URL.Path, g.suffix) {
			foundService = true
			break
		}
	}
	if !foundService {
		// The protocol spec in git/Documentation/technical/http-protocol.txt
		// says we must return 403 if no matching service is found.
		http.Error(w, "Forbidden", 403)
		return
	}

	// Ask the auth backend if the request is allowed, and what the
	// user ID (GL_ID) is.
	authResponse, err := h.doAuthRequest(r)
	if err != nil {
		fail500(w, "doAuthRequest", err)
		return
	}
	defer authResponse.Body.Close()

	if authResponse.StatusCode != 200 {
		// The Git request is not allowed by the backend. Maybe the
		// client needs to send HTTP Basic credentials.  Forward the
		// response from the auth backend to our client. This includes
		// the 'WWW-Authentication' header that acts as a hint that
		// Basic auth credentials are needed.
		for k, v := range authResponse.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(authResponse.StatusCode)
		io.Copy(w, authResponse.Body)
		return
	}

	// The auth backend validated the client request and told us who
	// the user is according to them (GL_ID). We must extract this
	// information from the auth response body.
	dec := json.NewDecoder(authResponse.Body)
	if err := dec.Decode(&env); err != nil {
		fail500(w, "decode JSON GL_ID", err)
		return
	}
	// Don't hog a TCP connection in CLOSE_WAIT, we can already close it now
	authResponse.Body.Close()

	// About path traversal: the Go net/http HTTP server, or
	// rather ServeMux, makes the following promise: "ServeMux
	// also takes care of sanitizing the URL request path, redirecting
	// any request containing . or .. elements to an equivalent
	// .- and ..-free URL.". In other words, we may assume that
	// r.URL.Path does not contain '/../', so there is no possibility
	// of path traversal here.
	repoPath := path.Join(h.repoRoot, strings.TrimSuffix(r.URL.Path, g.suffix))
	if !looksLikeRepo(repoPath) {
		http.Error(w, "Not Found", 404)
		return
	}

	g.handleFunc(env, g.rpc, repoPath, w, r)
}

func looksLikeRepo(p string) bool {
	// If /path/to/foo.git/objects exists then let's assume it is a valid Git
	// repository.
	if _, err := os.Stat(path.Join(p, "objects")); err != nil {
		log.Print(err)
		return false
	}
	return true
}

func (h *gitHandler) doAuthRequest(r *http.Request) (result *http.Response, err error) {
	url := h.authBackend + r.URL.RequestURI()
	authReq, err := http.NewRequest(r.Method, url, nil)
	if err != nil {
		return nil, err
	}
	// Forward all headers from our client to the auth backend. This includes
	// HTTP Basic authentication credentials (the 'Authorization' header).
	for k, v := range r.Header {
		authReq.Header[k] = v
	}
	return h.httpClient.Do(authReq)
}

func handleGetInfoRefs(env gitEnv, _ string, path string, w http.ResponseWriter, r *http.Request) {
	rpc := r.URL.Query().Get("service")
	if !(rpc == "git-upload-pack" || rpc == "git-receive-pack") {
		// The 'dumb' Git HTTP protocol is not supported
		http.Error(w, "Not Found", 404)
		return
	}

	// Prepare our Git subprocess
	cmd := gitCommand(env, "git", subCommand(rpc), "--stateless-rpc", "--advertise-refs", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail500(w, "handleGetInfoRefs", err)
		return
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		fail500(w, "handleGetInfoRefs", err)
		return
	}
	defer cleanUpProcessGroup(cmd) // Ensure brute force subprocess clean-up

	// Start writing the response
	w.Header().Add("Content-Type", fmt.Sprintf("application/x-%s-advertisement", rpc))
	w.Header().Add("Cache-Control", "no-cache")
	w.WriteHeader(200) // Don't bother with HTTP 500 from this point on, just return
	if err := pktLine(w, fmt.Sprintf("# service=%s\n", rpc)); err != nil {
		logContext("handleGetInfoRefs response", err)
		return
	}
	if err := pktFlush(w); err != nil {
		logContext("handleGetInfoRefs response", err)
		return
	}
	if _, err := io.Copy(w, stdout); err != nil {
		logContext("handleGetInfoRefs read from subprocess", err)
		return
	}
	if err := cmd.Wait(); err != nil {
		logContext("handleGetInfoRefs wait for subprocess", err)
		return
	}
}

func handlePostRPC(env gitEnv, rpc string, path string, w http.ResponseWriter, r *http.Request) {
	var body io.Reader
	var err error

	// The client request body may have been gzipped.
	if r.Header.Get("Content-Encoding") == "gzip" {
		body, err = gzip.NewReader(r.Body)
		if err != nil {
			fail500(w, "handlePostRPC", err)
			return
		}
	} else {
		body = r.Body
	}

	// Prepare our Git subprocess
	cmd := gitCommand(env, "git", subCommand(rpc), "--stateless-rpc", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail500(w, "handlePostRPC", err)
		return
	}
	defer stdout.Close()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail500(w, "handlePostRPC", err)
		return
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		fail500(w, "handlePostRPC", err)
		return
	}
	defer cleanUpProcessGroup(cmd) // Ensure brute force subprocess clean-up

	// Write the client request body to Git's standard input
	if _, err := io.Copy(stdin, body); err != nil {
		fail500(w, "handlePostRPC write to subprocess", err)
		return
	}
	stdin.Close()

	// Start writing the response
	w.Header().Add("Content-Type", fmt.Sprintf("application/x-%s-result", rpc))
	w.Header().Add("Cache-Control", "no-cache")
	w.WriteHeader(200) // Don't bother with HTTP 500 from this point on, just return
	if _, err := io.Copy(w, stdout); err != nil {
		logContext("handlePostRPC read from subprocess", err)
		return
	}
	if err := cmd.Wait(); err != nil {
		logContext("handlePostRPC wait for subprocess", err)
		return
	}
}

func fail500(w http.ResponseWriter, context string, err error) {
	http.Error(w, "Internal server error", 500)
	logContext(context, err)
}

func logContext(context string, err error) {
	log.Printf("%s: %v", context, err)
}

// Git subprocess helpers
func subCommand(rpc string) string {
	return strings.TrimPrefix(rpc, "git-")
}

func gitCommand(env gitEnv, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	// Start the command in its own process group (nice for signalling)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Explicitly set the environment for the Git command
	cmd.Env = []string{
		fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
		fmt.Sprintf("GL_ID=%s", env.GL_ID),
	}
	return cmd
}

func cleanUpProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	process := cmd.Process
	if process != nil && process.Pid > 0 {
		// Send SIGKILL (kill -9) to the process group of cmd
		syscall.Kill(-process.Pid, syscall.SIGKILL)
	}

	// reap our child process
	cmd.Wait()
}

// Git HTTP line protocol functions
func pktLine(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%04x%s", len(s)+4, s)
	return err
}

func pktFlush(w io.Writer) error {
	_, err := fmt.Fprint(w, "0000")
	return err
}