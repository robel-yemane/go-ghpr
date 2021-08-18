package ghpr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

// UpdateFunc is a callback function which should create a series of changes
// to the git WorkTree. These changes will be automatically committed on successful
// return by the PushCommit function
type UpdateFunc func(w *git.Worktree) (string, *object.Signature, error)

// Credentials represents a GitHub username and PAT
type Credentials struct {
	Username string
	Token    string
}

// Author represents information about the creator of a commit
type Author struct {
	Name  string
	Email string
}

// GithubPR GitHubPR is a container for all necessary state
type GithubPR struct {
	auth         http.BasicAuth
	filesystem   billy.Filesystem
	git          goGit
	gitHubClient *github.Client
	mergeSHA     string
	path         string
	pr           int
	gitRepo      *git.Repository
	owner        string
	repo         string
}

// goGit provides an interface for to go-git methods in use by this module
// This is interface is not exported.
type goGit interface {
	Clone(s storage.Storer, worktree billy.Filesystem, o *git.CloneOptions) (*git.Repository, error)
}

// realGoGit is a go-git backed implementation of the GoGit interface
type realGoGit struct {
}

func (ghpr realGoGit) Clone(s storage.Storer, worktree billy.Filesystem, o *git.CloneOptions) (*git.Repository, error) {
	return git.Clone(s, worktree, o)
}

// MakeGithubPR creates a new GithubPR struct with all the necessary state to clone, commit, raise a PR
// and merge. The repository will be cloned to a temporary directory in the current directory
func MakeGithubPR(repoName string, creds Credentials) (*GithubPR, error) {
	fs := osfs.New(".")
	return makeGithubPR(repoName, creds, &fs, realGoGit{})
}

// makeGithubPR is an internal function for creating a GithubPR instance. It allows injecting a mock filesystem
// and go-git implementation
func makeGithubPR(repoName string, creds Credentials, fs *billy.Filesystem, gogit goGit) (*GithubPR, error) {
	// A loose regex for a format of <user|org>/<repository>
	// Match one or more non-slash characters, followed by a slash,
	// followed by one or morer non-slash characters
	matched, err := regexp.MatchString("^[^/]+/[^/]+$", repoName)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, errors.New("invalid repository name supplied")
	}

	owner := strings.Split(repoName, "/")[0]
	repo := strings.Split(repoName, "/")[1]

	tempDir, err := util.TempDir(*fs, ".", "repo_")
	if err != nil {
		return nil, err
	}

	*fs, err = (*fs).Chroot(tempDir)
	if err != nil {
		return nil, err
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: creds.Token},
	)
	tc := oauth2.NewClient(context.Background(), ts)

	return &GithubPR{
		filesystem:   *fs,
		auth:         http.BasicAuth{Username: creds.Username, Password: creds.Token},
		path:         tempDir,
		gitHubClient: github.NewClient(tc),
		git:          gogit,
		repo:         repo,
		owner:        owner,
	}, nil
}

// Clone shallow clones the GitHub repository
func (ghpr *GithubPR) Clone() error {
	url := fmt.Sprintf("https://github.com/" + ghpr.owner + "/" + ghpr.repo)

	storageWorkTree, err := ghpr.filesystem.Chroot(".git")
	if err != nil {
		return err
	}

	// Pass a defafult LRU object cache, as per git.PlainClone's implementation
	ghpr.gitRepo, err = ghpr.git.Clone(
		filesystem.NewStorage(storageWorkTree, cache.NewObjectLRUDefault()),
		ghpr.filesystem,
		&git.CloneOptions{
			Depth: 1,
			URL:   url,
			Auth:  &ghpr.auth})

	if err != nil {
		return err
	}

	return nil
}

// PushCommit creates a commit for the Worktree changes made by the UpdateFunc parameter
// and pushes that branch to the remote origin server
func (ghpr *GithubPR) PushCommit(branchName string, fn UpdateFunc) error {
	headRef, err := ghpr.gitRepo.Head()
	if err != nil {
		return err
	}

	branchRef := fmt.Sprintf("refs/heads/%s", branchName)
	ref := plumbing.NewHashReference(plumbing.ReferenceName(branchRef), headRef.Hash())
	err = ghpr.gitRepo.Storer.SetReference(ref)
	if err != nil {
		return err
	}

	w, err := ghpr.gitRepo.Worktree()
	if err != nil {
		return err
	}

	w.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branchName)})

	commitMessage, author, err := fn(w)
	if err != nil {
		return err
	}

	// If no commit time is set (i.e. defaulted to the epoch), use the current time
	if author.When.Equal(time.Time{}) {
		author.When = time.Now()
	}

	_, err = w.Commit(commitMessage, &git.CommitOptions{Author: author})
	if err != nil {
		return err
	}

	branchRef = fmt.Sprintf("refs/remotes/origin/%s", branchName)
	ref = plumbing.NewHashReference(plumbing.ReferenceName(branchRef), headRef.Hash())
	err = ghpr.gitRepo.Storer.SetReference(ref)
	if err != nil {
		return err
	}

	err = ghpr.gitRepo.Push(&git.PushOptions{
		Auth: &ghpr.auth,
	})
	return err
}

// RaisePR creates a pull request from the sourceBranch (HEAD) to the targetBranch (base)
func (ghpr *GithubPR) RaisePR(sourceBranch string, targetBranch string, title string, body string) error {
	pr, _, err := ghpr.gitHubClient.PullRequests.Create(context.Background(),
		ghpr.owner, ghpr.repo,
		&github.NewPullRequest{
			Title: &title,
			Head:  &sourceBranch,
			Base:  &targetBranch,
			Body:  &body})
	if err != nil {
		return err
	}

	ghpr.pr = *pr.Number

	return err
}

func (ghpr *GithubPR) waitForStatus(shaRef string, owner string, repo string, statusContext string) error {
	c1 := make(chan error, 1)
	go func() {
		fmt.Printf("Waiting for %s to become mergeable\n", shaRef)
		for {
			time.Sleep(time.Second * 2)
			statuses, _, err := ghpr.gitHubClient.Repositories.ListStatuses(context.Background(), owner, repo,
				shaRef, &github.ListOptions{PerPage: 20})

			if err != nil {
				c1 <- err
				return
			}

			if statuses != nil {
				for i := 0; i < len(statuses); i++ {
					context := statuses[i].GetContext()
					state := statuses[i].GetState()

					if context == statusContext {
						if state == "success" {
							c1 <- nil
							return
						}
						if state == "failure" || state == "error" {
							c1 <- errors.New("target status check is in a failed state, aborting")
							return
						}
					}
				}
			}
		}
	}()

	select {
	case err := <-c1:
		return err
	case <-time.After(60 * time.Minute):
		return errors.New("timed out waiting for PR to become mergeable")
	}
}

// WaitForPR waits until the raised PR passes the supplied status check. It returns
// an error if a failed or errored state is encountered
func (ghpr *GithubPR) WaitForPR(statusContext string) error {
	pr, _, err := ghpr.gitHubClient.PullRequests.Get(context.Background(), ghpr.owner, ghpr.repo, ghpr.pr)
	if err != nil {
		return err
	}

	fmt.Printf("HEAD sha is %s\n", *pr.Head.SHA)
	return ghpr.waitForStatus(*pr.Head.SHA, ghpr.owner, ghpr.repo, statusContext)

}

// MergePR merges a PR, provided it is in a mergeable state, otherwise returning
// an error
func (ghpr *GithubPR) MergePR() error {
	pr, _, err := ghpr.gitHubClient.PullRequests.Get(context.Background(), ghpr.owner, ghpr.repo, ghpr.pr)
	if err != nil {
		return err
	}

	if pr.Mergeable != nil && *pr.Mergeable {
		merge, _, err := ghpr.gitHubClient.PullRequests.Merge(context.Background(), ghpr.owner, ghpr.repo, *pr.Number, "", &github.PullRequestOptions{MergeMethod: "merge"})
		if err != nil {
			return err
		}
		ghpr.mergeSHA = *merge.SHA
	} else {
		return errors.New("PR is not mergeable")
	}
	return nil
}

// WaitForMergeCommit waits for the merge commit to receive a successful state
// for the supplied status check. It returns an error if a failed or errored
// state is encountered
func (ghpr *GithubPR) WaitForMergeCommit(statusContext string) error {
	return ghpr.waitForStatus(ghpr.mergeSHA, ghpr.owner, ghpr.repo, statusContext)
}

// Close removes the cloned repository from the filesystem
func (ghpr *GithubPR) Close() error {
	return os.RemoveAll(ghpr.path)
}

func (ghpr *GithubPR) Create(branchName string, targetBranch string, prStatusContext string, masterStatusContext string, fn UpdateFunc) error {
	err := ghpr.Clone()
	defer ghpr.Close()
	if err != nil {
		return err
	}

	err = ghpr.PushCommit(branchName, fn)
	if err != nil {
		return err
	}

	stuff := "test"
	err = ghpr.RaisePR(branchName, targetBranch, stuff, "")
	if err != nil {
		return err
	}

	err = ghpr.WaitForPR(prStatusContext)
	if err != nil {
		return err
	}

	err = ghpr.MergePR()
	if err != nil {
		return err
	}

	return ghpr.WaitForMergeCommit(masterStatusContext)
}
