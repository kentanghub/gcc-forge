package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Repo struct {
	Name          string
	Owner         string
	Description   string
	DefaultBranch string
	CreatedAt     time.Time
}

type GitObject struct {
	Type string
	Data []byte
}

type Ref struct {
	Name string
	Sha  string
}

type Issue struct {
	Number    int
	Repo      string
	Title     string
	Body      string
	State     string
	Labels    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PullRequest struct {
	Number        int
	Repo          string
	Title         string
	Body          string
	Head          string
	Base          string
	State         string
	Merged        bool
	MergeCommitSha string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type FileEntry struct {
	Path    string
	Sha     string
	Content string
	Size    int
}

type Store struct {
	mu           sync.RWMutex
	repos        map[string]*Repo
	objects      map[string]*GitObject
	refs         map[string][]Ref
	issues       map[string][]Issue
	pulls        map[string][]PullRequest
	labels       map[string][]string
	files        map[string]map[string]*FileEntry
	issueCounter map[string]int
	pullCounter  map[string]int
	tokenOwner   string
}

func NewStore() *Store {
	return &Store{
		repos:        make(map[string]*Repo),
		objects:      make(map[string]*GitObject),
		refs:         make(map[string][]Ref),
		issues:       make(map[string][]Issue),
		pulls:        make(map[string][]PullRequest),
		labels:       make(map[string][]string),
		files:        make(map[string]map[string]*FileEntry),
		issueCounter: make(map[string]int),
		pullCounter:  make(map[string]int),
		tokenOwner:   "shop",
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) CreateRepo(owner, name, desc string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; ok {
		return nil
	}
	r := &Repo{Name: name, Owner: owner, Description: desc, DefaultBranch: "main", CreatedAt: time.Now()}
	s.repos[key] = r
	s.refs[key] = []Ref{}
	s.issues[key] = []Issue{}
	s.pulls[key] = []PullRequest{}
	s.labels[key] = []string{}
	s.files[key] = make(map[string]*FileEntry)
	s.issueCounter[key] = 1
	s.pullCounter[key] = 1
	return r
}

func (s *Store) GetRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[repoKey(owner, name)]
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

func (s *Store) ListUserRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Repo
	for key, r := range s.repos {
		if strings.HasPrefix(key, owner+"/") {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *Store) HashObject(data []byte, typ string) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	sha := hex.EncodeToString(h.Sum(nil))
	s.objects[sha] = &GitObject{Type: typ, Data: data}
	return sha
}

func (s *Store) GetObject(sha string) *GitObject {
	return s.objects[sha]
}

func (s *Store) SetRef(owner, name, refName, sha string) {
	key := repoKey(owner, name)
	refs := s.refs[key]
	for i, r := range refs {
		if r.Name == refName {
			refs = append(refs[:i], refs[i+1:]...)
			break
		}
	}
	refs = append(refs, Ref{Name: refName, Sha: sha})
	s.refs[key] = refs
}

func (s *Store) GetRef(owner, name, refName string) string {
	for _, r := range s.refs[repoKey(owner, name)] {
		if r.Name == refName {
			return r.Sha
		}
	}
	return ""
}

func (s *Store) GetRefs(owner, name string) []Ref {
	return s.refs[repoKey(owner, name)]
}

func (s *Store) CreateIssue(owner, name, title, body string, labels []string) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return nil, fmt.Errorf("not found")
	}
	existingLabels := s.labels[key]
	for _, l := range labels {
		found := false
		for _, el := range existingLabels {
			if el == l {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("label %q does not exist", l)
		}
	}
	num := s.issueCounter[key]
	s.issueCounter[key] = num + 1
	issue := Issue{
		Number: num, Repo: key, Title: title, Body: body,
		State: "open", Labels: labels,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.issues[key] = append(s.issues[key], issue)
	return &issue, nil
}

func (s *Store) GetIssue(owner, name string, num int) *Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, i := range s.issues[repoKey(owner, name)] {
		if i.Number == num {
			return &i
		}
	}
	return nil
}

func (s *Store) ListIssues(owner, name, state string) []Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.issues[repoKey(owner, name)]
	if state == "" {
		return all
	}
	var result []Issue
	for _, i := range all {
		if i.State == state {
			result = append(result, i)
		}
	}
	return result
}

func (s *Store) CreateLabel(owner, name, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	for _, l := range s.labels[key] {
		if l == label {
			return
		}
	}
	s.labels[key] = append(s.labels[key], label)
}

func (s *Store) CreatePull(owner, name, title, head, base, body string) *PullRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	num := s.pullCounter[key]
	s.pullCounter[key] = num + 1
	pr := PullRequest{
		Number: num, Repo: key, Title: title, Body: body,
		Head: head, Base: base, State: "open", Merged: false,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.pulls[key] = append(s.pulls[key], pr)
	return &pr
}

func (s *Store) GetPull(owner, name string, num int) *PullRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.pulls[repoKey(owner, name)] {
		if p.Number == num {
			return &p
		}
	}
	return nil
}

func (s *Store) MergePull(owner, name string, num int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	for i := range s.pulls[key] {
		if s.pulls[key][i].Number == num {
			pr := &s.pulls[key][i]
			if pr.State == "closed" {
				return "", fmt.Errorf("pull request is closed")
			}
			mergeSha := fmt.Sprintf("%040x", sha1.Sum([]byte(fmt.Sprintf("merge-%s-%d-%d", key, num, time.Now().UnixNano()))))
			pr.State = "closed"
			pr.Merged = true
			pr.MergeCommitSha = mergeSha
			pr.UpdatedAt = time.Now()
			s.SetRef(owner, name, "refs/heads/"+pr.Base, mergeSha)
			return mergeSha, nil
		}
	}
	return "", fmt.Errorf("not found")
}

func (s *Store) PutFile(owner, name, path, content string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if s.files[key] == nil {
		s.files[key] = make(map[string]*FileEntry)
	}
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("blob %d\x00", len(content))))
	h.Write([]byte(content))
	sha := hex.EncodeToString(h.Sum(nil))
	s.files[key][path] = &FileEntry{Path: path, Sha: sha, Content: content, Size: len(content)}
	return sha
}

func (s *Store) GetFile(owner, name, path string) *FileEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if fm, ok := s.files[repoKey(owner, name)]; ok {
		return fm[path]
	}
	return nil
}

func (s *Store) DeleteFile(owner, name, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if fm, ok := s.files[key]; ok {
		if _, exists := fm[path]; exists {
			delete(fm, path)
			return true
		}
	}
	return false
}