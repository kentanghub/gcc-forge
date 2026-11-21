package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var store = NewStore()
var requestID atomic.Uint64

// ---------- helpers ----------

func nextID() string {
	return fmt.Sprintf("%08x", requestID.Add(1))
}

func jsonResp(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", nextID())
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)+1))
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte{'\n'})
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	return body, err
}

func parseRepo(path string) (owner, name string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ---------- git protocol ----------

func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func gitRefsToPktLines(refs []Ref) string {
	var b strings.Builder
	for _, r := range refs {
		b.WriteString(pktLine(fmt.Sprintf("%s %s\n", r.Sha, r.Name)))
	}
	b.WriteString("0000")
	return b.String()
}

func handleGitInfoRefs(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/info/refs"), "/")
	if len(parts) < 3 {
		jsonResp(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	owner := parts[1]
	name := strings.TrimSuffix(parts[2], ".git")
	repo := store.GetRepo(owner, name)
	if repo == nil {
		jsonResp(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	service := r.URL.Query().Get("service")
	refs := store.GetRefs(owner, name)

	if service == "git-upload-pack" {
		body := "# service=git-upload-pack\n0000"
		body += "version 2\n"
		body += "agent=gcc-forge\n"
		body += "ls-refs\nfetch\n"
		body += "0000"
		body += gitRefsToPktLines(refs)
		body += "0000"
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write([]byte(body))
	} else if service == "git-receive-pack" {
		body := "# service=git-receive-pack\n0000"
		for _, r := range refs {
			body += pktLine(fmt.Sprintf("%s %s\x00 report-status side-band-64k delete-refs\n", r.Sha, r.Name))
		}
		body += "0000"
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write([]byte(body))
	} else {
		jsonResp(w, 400, map[string]string{"message": "Unknown service"})
	}
}

func handleGitUploadPack(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	bodyStr := string(body)

	if strings.Contains(bodyStr, "command=ls-refs") {
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/git-upload-pack"), "/")
		if len(parts) < 3 {
			return
		}
		owner := parts[1]
		name := strings.TrimSuffix(parts[2], ".git")
		refs := store.GetRefs(owner, name)
		out := gitRefsToPktLines(refs) + "0000"
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(200)
		w.Write([]byte(out))
	} else {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(200)
		w.Write([]byte("0000"))
	}
}

func handleGitReceivePack(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/git-receive-pack"), "/")
	if len(parts) < 3 {
		return
	}
	owner := parts[1]
	name := strings.TrimSuffix(parts[2], ".git")

	var result strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		if ln, _ := hex.DecodeString(line[:4]); len(ln) == 2 {
			payload := line[4:]
			parts2 := strings.SplitN(payload, " ", 3)
			if len(parts2) == 3 && len(parts2[0]) == 40 && len(parts2[1]) == 40 {
				oldSha, newSha, refName := parts2[0], parts2[1], parts2[2]
				refName = strings.TrimSpace(refName)
				if newSha != "0000000000000000000000000000000000000000" {
					currentSha := store.GetRef(owner, name, refName)
					if currentSha != "" && currentSha != oldSha && oldSha != "0000000000000000000000000000000000000000" {
						result.WriteString(pktLine(fmt.Sprintf("ng %s non-fast-forward\n", refName)))
						continue
					}
					commitMsg := fmt.Sprintf("push to %s", refName)
					commitData := []byte(fmt.Sprintf("tree %s\n\n%s\n", hex.EncodeToString(make([]byte, 20)), commitMsg))
					newSha = store.HashObject(commitData, "commit")
					store.SetRef(owner, name, refName, newSha)
					result.WriteString(pktLine(fmt.Sprintf("ok %s\n", refName)))
				} else {
					refs := store.GetRefs(owner, name)
					var filtered []Ref
					for _, r := range refs {
						if r.Name != refName {
							filtered = append(filtered, r)
						}
					}
					store.refs[repoKey(owner, name)] = filtered
					result.WriteString(pktLine(fmt.Sprintf("ok %s\n", refName)))
				}
			}
		}
	}
	result.WriteString("0000")
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	w.Write([]byte(result.String()))
}

// ---------- REST API ----------

func repoToJSON(r *Repo) map[string]interface{} {
	h := sha1.Sum([]byte(r.Owner + "/" + r.Name))
	id := hex.EncodeToString(h[:4])
	return map[string]interface{}{
		"id":              id,
		"node_id":         id,
		"name":            r.Name,
		"full_name":       r.Owner + "/" + r.Name,
		"owner":           map[string]interface{}{"login": r.Owner, "id": 1},
		"private":         false,
		"description":     r.Description,
		"default_branch":  r.DefaultBranch,
		"created_at":      r.CreatedAt.Format(time.RFC3339),
		"updated_at":      r.CreatedAt.Format(time.RFC3339),
		"has_issues":      true,
		"open_issues_count": 0,
	}
}

func issueToJSON(i *Issue) map[string]interface{} {
	parts := strings.SplitN(i.Repo, "/", 2)
	var labels []map[string]string
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]interface{}{
		"id":       i.Number,
		"number":   i.Number,
		"title":    i.Title,
		"body":     i.Body,
		"state":    i.State,
		"labels":   labels,
		"created_at": i.CreatedAt.Format(time.RFC3339),
		"updated_at": i.UpdatedAt.Format(time.RFC3339),
		"user":     map[string]string{"login": parts[0]},
	}
}

func handleREST(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/v3") {
		return false
	}

	if r.Method == "GET" && (path == "/api/v3" || path == "/api/v3/") {
		jsonResp(w, 200, map[string]string{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		})
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		return true
	}

	if r.Method == "GET" && path == "/api/v3/meta" {
		jsonResp(w, 200, map[string]interface{}{
			"installed_version": "1.0.0",
			"verifiable_password_authentication": true,
		})
		return true
	}

	if r.Method == "GET" && path == "/api/v3/user/repos" {
		repos := store.ListUserRepos("shop")
		var out []map[string]interface{}
		for _, r := range repos {
			out = append(out, repoToJSON(r))
		}
		jsonResp(w, 200, out)
		return true
	}

	if r.Method == "POST" && path == "/api/v3/user/repos" {
		body, _ := readBody(r)
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		json.Unmarshal(body, &req)
		repo := store.CreateRepo("shop", req.Name, req.Description)
		if repo == nil {
			jsonResp(w, 422, map[string]string{"message": "Repository exists"})
			return true
		}
		jsonResp(w, 201, repoToJSON(repo))
		return true
	}

	// /api/v3/repos/:owner/:name routes
	rest := strings.TrimPrefix(path, "/api/v3/repos/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return false
	}
	owner, repoName := parts[0], parts[1]
	sub := ""
	if len(parts) > 2 {
		sub = "/" + parts[2]
	}

	repo := store.GetRepo(owner, repoName)

	if sub == "" && r.Method == "GET" {
		if repo == nil {
			jsonResp(w, 404, map[string]string{"message": "Not Found"})
			return true
		}
		jsonResp(w, 200, repoToJSON(repo))
		return true
	}

	if sub == "" && r.Method == "PATCH" {
		if repo == nil {
			jsonResp(w, 404, map[string]string{"message": "Not Found"})
			return true
		}
		body, _ := readBody(r)
		var req struct{ Description string }
		json.Unmarshal(body, &req)
		if req.Description != "" {
			repo.Description = req.Description
		}
		jsonResp(w, 200, repoToJSON(repo))
		return true
	}

	if sub == "" && r.Method == "DELETE" {
		store.DeleteRepo(owner, repoName)
		w.WriteHeader(204)
		return true
	}

	// Issues
	if strings.HasPrefix(sub, "/issues") {
		if sub == "/issues" && r.Method == "POST" {
			body, _ := readBody(r)
			var req struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			json.Unmarshal(body, &req)
			issue, err := store.CreateIssue(owner, repoName, req.Title, req.Body, req.Labels)
			if err != nil {
				jsonResp(w, 422, map[string]string{"message": err.Error()})
				return true
			}
			jsonResp(w, 201, issueToJSON(issue))
			return true
		}

		if strings.Count(sub, "/") == 1 && r.Method == "GET" {
			numStr := strings.TrimPrefix(sub, "/issues/")
			num, _ := strconv.Atoi(numStr)
			issue := store.GetIssue(owner, repoName, num)
			if issue == nil {
				jsonResp(w, 404, map[string]string{"message": "Not Found"})
				return true
			}
			jsonResp(w, 200, issueToJSON(issue))
			return true
		}
	}

	// Labels
	if sub == "/labels" && r.Method == "POST" {
		body, _ := readBody(r)
		var req struct{ Name string }
		json.Unmarshal(body, &req)
		store.CreateLabel(owner, repoName, req.Name)
		jsonResp(w, 201, map[string]string{"name": req.Name})
		return true
	}

	// Pull requests
	if sub == "/pulls" && r.Method == "POST" {
		body, _ := readBody(r)
		var req struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
		}
		json.Unmarshal(body, &req)
		pr := store.CreatePull(owner, repoName, req.Title, req.Head, req.Base, req.Body)
		jsonResp(w, 201, map[string]interface{}{
			"id":     pr.Number,
			"number": pr.Number,
			"title":  pr.Title,
			"state":  pr.State,
			"head":   map[string]string{"ref": pr.Head},
			"base":   map[string]string{"ref": pr.Base},
			"created_at": pr.CreatedAt.Format(time.RFC3339),
		})
		return true
	}

	if strings.Count(sub, "/") == 1 && strings.HasPrefix(sub, "/pulls/") {
		numStr := strings.TrimPrefix(sub, "/pulls/")
		if strings.Contains(numStr, "/merge") && r.Method == "PUT" {
			numStr = strings.TrimSuffix(numStr, "/merge")
			num, _ := strconv.Atoi(numStr)
			sha, err := store.MergePull(owner, repoName, num)
			if err != nil {
				jsonResp(w, 405, map[string]string{"message": err.Error()})
				return true
			}
			jsonResp(w, 200, map[string]interface{}{"sha": sha, "merged": true, "message": "Pull request merged"})
			return true
		}
		num, _ := strconv.Atoi(numStr)
		pr := store.GetPull(owner, repoName, num)
		if pr == nil {
			jsonResp(w, 404, map[string]string{"message": "Not Found"})
			return true
		}
		jsonResp(w, 200, map[string]interface{}{
			"id": pr.Number, "number": pr.Number, "title": pr.Title, "state": pr.State,
			"head": map[string]string{"ref": pr.Head}, "base": map[string]string{"ref": pr.Base},
			"merged": pr.Merged, "merge_commit_sha": pr.MergeCommitSha,
		})
		return true
	}

	// Contents
	if strings.HasPrefix(sub, "/contents/") && r.Method == "PUT" {
		fpath := strings.TrimPrefix(sub, "/contents/")
		body, _ := readBody(r)
		var req struct {
			Content string `json:"content"`
			SHA     string `json:"sha"`
		}
		json.Unmarshal(body, &req)
		decoded, _ := base64.StdEncoding.DecodeString(req.Content)
		existing := store.GetFile(owner, repoName, fpath)
		status := 201
		if existing != nil {
			if req.SHA != "" && existing.Sha != req.SHA {
				jsonResp(w, 409, map[string]string{"message": "SHA mismatch"})
				return true
			}
			status = 200
		}
		sha := store.PutFile(owner, repoName, fpath, string(decoded))
		jsonResp(w, status, map[string]interface{}{"content": map[string]string{"sha": sha, "path": fpath}})
		return true
	}

	if strings.HasPrefix(sub, "/contents/") && r.Method == "DELETE" {
		fpath := strings.TrimPrefix(sub, "/contents/")
		if !store.DeleteFile(owner, repoName, fpath) {
			jsonResp(w, 404, map[string]string{"message": "Not Found"})
			return true
		}
		jsonResp(w, 200, map[string]interface{}{"commit": map[string]string{"sha": fmt.Sprintf("%040x", sha1.Sum([]byte(fpath)))}})
		return true
	}

	return false
}

// ---------- GraphQL ----------

func handleGraphQL(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/graphql" {
		return false
	}
	body, _ := readBody(r)
	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	json.Unmarshal(body, &req)
	q := strings.TrimSpace(req.Query)

	result := map[string]interface{}{}

	if strings.Contains(q, "__type") {
		result["data"] = map[string]interface{}{
			"__type": map[string]string{"name": "Issue", "kind": "OBJECT"},
		}
	} else if strings.Contains(q, "viewer") && strings.Contains(q, "repositories") {
		repos := store.ListUserRepos("shop")
		var nodes []map[string]interface{}
		for _, r := range repos {
			nodes = append(nodes, map[string]interface{}{
				"name": r.Name, "nameWithOwner": r.Owner + "/" + r.Name,
				"owner": map[string]string{"login": r.Owner},
				"defaultBranchRef": map[string]string{"name": r.DefaultBranch},
				"viewerPermission": "ADMIN",
				"hasIssuesEnabled": true,
			})
		}
		result["data"] = map[string]interface{}{"viewer": map[string]interface{}{"repositories": map[string]interface{}{"nodes": nodes}}}
	} else if strings.Contains(q, "repository(") {
		result["data"] = map[string]interface{}{"repository": nil}
	} else if strings.Contains(q, "search(") {
		result["data"] = map[string]interface{}{"search": map[string]interface{}{"edges": []interface{}{}, "issueCount": 0}}
	} else {
		result["data"] = nil
	}

	jsonResp(w, 200, result)
	return true
}

// ---------- MCP ----------

var mcpTools = []map[string]interface{}{
	{"name": "create_repo", "description": "Create a new repository",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]string{"type": "string"},
			},
			"required": []string{"name"},
		}},
	{"name": "get_repo", "description": "Get repository details",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]string{"type": "string"},
				"repo":  map[string]string{"type": "string"},
			},
			"required": []string{"owner", "repo"},
		}},
	{"name": "create_issue", "description": "Create an issue",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]string{"type": "string"},
				"repo":  map[string]string{"type": "string"},
				"title": map[string]string{"type": "string"},
				"body":  map[string]string{"type": "string"},
			},
			"required": []string{"owner", "repo", "title"},
		}},
	{"name": "list_issues", "description": "List repository issues",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]string{"type": "string"},
				"repo":  map[string]string{"type": "string"},
				"state": map[string]string{"type": "string"},
			},
			"required": []string{"owner", "repo"},
		}},
	{"name": "create_pull_request", "description": "Create a pull request",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]string{"type": "string"},
				"repo":  map[string]string{"type": "string"},
				"title": map[string]string{"type": "string"},
				"head":  map[string]string{"type": "string"},
				"base":  map[string]string{"type": "string"},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		}},
	{"name": "merge_pull_request", "description": "Merge a pull request",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner":       map[string]string{"type": "string"},
				"repo":        map[string]string{"type": "string"},
				"pull_number": map[string]string{"type": "number"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		}},
}

func handleMCP(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/mcp" {
		return false
	}
	body, _ := readBody(r)
	var req struct {
		JSONRPC string                 `json:"jsonrpc"`
		ID      interface{}            `json:"id"`
		Method  string                 `json:"method"`
		Params  map[string]interface{} `json:"params"`
	}
	json.Unmarshal(body, &req)

	if req.Method == "initialize" {
		jsonResp(w, 200, map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{
				"protocolVersion": "2026-07-28",
				"capabilities":    map[string]interface{}{"tools": map[string]bool{}},
				"serverInfo":      map[string]string{"name": "gcc-forge", "version": "1.0.0"},
			},
		})
		return true
	}
	if req.Method == "notifications/initialized" {
		jsonResp(w, 200, map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": map[string]interface{}{}})
		return true
	}
	if req.Method == "tools/list" {
		jsonResp(w, 200, map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID, "result": map[string]interface{}{"tools": mcpTools},
		})
		return true
	}
	if req.Method == "tools/call" {
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		owner, _ := args["owner"].(string)
		repoN, _ := args["repo"].(string)
		var result interface{}
		switch name {
		case "create_repo":
			rname, _ := args["name"].(string)
			result = store.CreateRepo(owner, rname, "")
		case "get_repo":
			result = store.GetRepo(owner, repoN)
		case "create_issue":
			title, _ := args["title"].(string)
			bodyStr, _ := args["body"].(string)
			issue, _ := store.CreateIssue(owner, repoN, title, bodyStr, nil)
			result = issue
		case "list_issues":
			state, _ := args["state"].(string)
			result = store.ListIssues(owner, repoN, state)
		case "create_pull_request":
			title, _ := args["title"].(string)
			head, _ := args["head"].(string)
			base, _ := args["base"].(string)
			bodyStr, _ := args["body"].(string)
			result = store.CreatePull(owner, repoN, title, head, base, bodyStr)
		case "merge_pull_request":
			pn, _ := args["pull_number"].(float64)
			sha, _ := store.MergePull(owner, repoN, int(pn))
			result = map[string]string{"sha": sha, "merged": "true"}
		default:
			jsonResp(w, 200, map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "error": map[string]interface{}{"code": -32601, "message": "Tool not found"}})
			return true
		}
		resultJSON, _ := json.Marshal(result)
		jsonResp(w, 200, map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": string(resultJSON)}},
			},
		})
		return true
	}
	if req.Method == "server/discover" {
		jsonResp(w, 200, map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID, "result": map[string]interface{}{"supportedVersions": []string{"2026-07-28"}},
		})
		return true
	}
	jsonResp(w, 200, map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "error": map[string]interface{}{"code": -32601, "message": "Method not found"}})
	return true
}

// ---------- main ----------

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Health check
		if r.Method == "GET" && path == "/healthz" {
			jsonResp(w, 200, map[string]bool{"ok": true})
			return
		}

		// REST v3
		if handleREST(w, r) {
			return
		}

		// GraphQL
		if handleGraphQL(w, r) {
			return
		}

		// MCP
		if handleMCP(w, r) {
			return
		}

		// Git smart HTTP
		if strings.HasSuffix(path, "/info/refs") {
			if r.Header.Get("Authorization") == "" {
				jsonResp(w, 401, map[string]string{"message": "Requires authentication"})
				return
			}
			handleGitInfoRefs(w, r)
			return
		}
		if strings.HasSuffix(path, "/git-upload-pack") {
			handleGitUploadPack(w, r)
			return
		}
		if strings.HasSuffix(path, "/git-receive-pack") {
			handleGitReceivePack(w, r)
			return
		}

		// 404
		jsonResp(w, 404, map[string]string{"message": "Not Found"})
	})

	// Register with the suite
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		log.Fatal("MAKEFUN_ENV_URL is not set")
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	log.Printf("listening on port %d", port)

	go func() {
		time.Sleep(100 * time.Millisecond)
		// Register
		regBody, _ := json.Marshal(map[string]int{"port": port})
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(regBody))
		if err == nil {
			resp.Body.Close()
		}
		log.Printf("registered with env")
		// Poll done
		for {
			resp, err := http.Get(envURL + "/done")
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			var done struct{ Done bool }
			json.NewDecoder(resp.Body).Decode(&done)
			resp.Body.Close()
			if done.Done {
				log.Printf("done, exiting")
				os.Exit(0)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	// Use random for unique IDs
	rand.Seed(time.Now().UnixNano())

	http.Serve(listener, mux)
}