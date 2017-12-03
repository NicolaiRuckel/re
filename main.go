// Partially derived from github.com/rsc/github.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

var (
	project      = flag.String("p", "cockroachdb/cockroach", "GitHub owner/repo name")
	tokenFile    = flag.String("token", "", "read GitHub token personal access token from `file` (default $HOME/.github-issue-token)")
	projectOwner = ""
	projectRepo  = ""
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: re [-p owner/repo] pr

If query is a single number, prints the full history for the issue.
Otherwise, prints a table of matching results.
`)
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
	}

	q := strings.Join(flag.Args(), " ")
	switch q {
	case "out":
	case "in":
	}

	f := strings.Split(*project, "/")
	if len(f) != 2 {
		log.Fatal("invalid form for -p argument: must be owner/repo, like golang/go")
	}
	projectOwner = f[0]
	projectRepo = f[1]

	loadAuth()

	ctx := context.Background()

	n, _ := strconv.Atoi(q)
	if n != 0 {
		cmd := exec.Command("git", "fetch", "https://github.com/cockroachdb/cockroach", fmt.Sprintf("refs/pull/%d/head:reviews/pr/%d", n, n))

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		log.Printf("Fetching refs for PR %d", n)
		if err := cmd.Run(); err != nil {
			log.Fatal(fmt.Errorf("invoking fetch: %v", err))
		}

		log.Printf("Fetching details for PR %d", n)
		pr, _, err := client.PullRequests.Get(ctx, projectOwner, projectRepo, n)
		if err != nil {
			log.Fatal(err)
		}

		buf := bytes.NewBuffer(make([]byte, 0, 1024))
		printPR(ctx, buf, pr)

		base := *pr.Base.SHA
		head := *pr.Head.SHA

		pretty := `--pretty=tformat:commit %H%nAuthor: %an <%ae>%nDate:   %ad%n%n%w(0,4,4)%B`
		cmd = exec.Command("git", "show", "--reverse", pretty, fmt.Sprintf("%s..%s", base, head))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			log.Fatal(err)
		}
		if _, err := buf.ReadFrom(stdout); err != nil {
			log.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			log.Fatal(err)
		}

		updated := editText(buf.Bytes())

		request, err := parseFile(updated)
		if err != nil {
			log.Fatal(err)
		}

		postComments(ctx, n, request)
	}
}

const timeFormat = "2006-01-02 15:04:05"

func printPR(ctx context.Context, w io.Writer, pr *github.PullRequest) error {
	// Fool Vim's filetype detector for Git commit messages
	fmt.Fprint(w, "commit 0000000000000000000000000000000000000000\n")
	fmt.Fprintf(w, "Author: %s\n", getUserLogin(pr.User))
	fmt.Fprintf(w, "Date:   %s\n", getTime(pr.CreatedAt).Format(timeFormat))
	fmt.Fprintf(w, "Title:  %s\n", getString(pr.Title))
	fmt.Fprintf(w, "State:  %s\n", getString(pr.State))
	if pr.MergedAt != nil {
		fmt.Fprintf(w, "Merged: %s\n", getTime(pr.MergedAt).Format(timeFormat))
	}
	if pr.ClosedAt != nil {
		fmt.Fprintf(w, "Closed: %s\n", getTime(pr.ClosedAt).Format(timeFormat))
	}
	fmt.Fprintf(w, "URL:    https://github.com/%s/%s/pulls/%d\n", projectOwner, projectRepo, getInt(pr.Number))

	fmt.Fprintf(w, "\nCreated by %s (%s)\n", getUserLogin(pr.User), getTime(pr.CreatedAt).Format(timeFormat))
	if pr.Body != nil {
		text := strings.TrimSpace(*pr.Body)
		if text != "" {
			fmt.Fprintf(w, "\n\t%s\n", wrap(text, "\t"))
		}
	}

	for page := 1; ; {
		list, resp, err := client.Issues.ListComments(ctx, projectOwner, projectRepo, getInt(pr.Number), &github.IssueListCommentsOptions{
			ListOptions: github.ListOptions{
				Page:    page,
				PerPage: 100,
			},
		})
		for _, com := range list {
			fmt.Fprintf(w, "\nComment by %s (%s)\n", getUserLogin(com.User), getTime(com.CreatedAt).Format(timeFormat))
			if com.Body != nil {
				text := strings.TrimSpace(*com.Body)
				if text != "" {
					fmt.Fprintf(w, "\n\t%s\n", wrap(text, "\t"))
				}
			}
		}
		if err != nil {
			return err
		}
		if resp.NextPage < page {
			break
		}
		page = resp.NextPage
	}
	fmt.Fprint(w, "\n")
	fmt.Fprintf(w, `
# Add top-level review comments by typing between the marker lines below.
# Don't modify the markers!
# Approve this PR by typing "APPROVE" on a line by itself.
# Request changes on this PR by typing "DENY" on a line by itself.

%s
%s

# Add ordinary review comments by typing on a new line below the line of the
# diff you'd like to comment on. Comments may not begin with the special
# characters <space>, +, -, @, or *.
#
# Pre-existing comments are prefixed with *.

`, topLevelStartMarker, topLevelEndMarker)
	return nil
}

var (
	topLevelStartMarker = "# ------ BEGIN  TOP-LEVEL REVIEW COMMENTS ----- #"
	topLevelEndMarker   = "# ------ END OF TOP-LEVEL REVIEW COMMENTS ----- #"
)

func editText(original []byte) []byte {
	f, err := ioutil.TempFile("", "re-edit-")
	if err != nil {
		log.Fatal(err)
	}
	if err := ioutil.WriteFile(f.Name(), original, 0666); err != nil {
		log.Fatal(err)
	}
	if err := runEditor(f.Name()); err != nil {
		log.Fatal(err)
	}
	updated, err := ioutil.ReadFile(f.Name())
	if err != nil {
		log.Fatal(err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return updated
}

func runEditor(filename string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "ed"
	}

	// If the editor contains spaces or other magic shell chars,
	// invoke it as a shell command. This lets people have
	// environment variables like "EDITOR=emacs -nw".
	// The magic list of characters and the idea of running
	// sh -c this way is taken from git/run-command.c.
	var cmd *exec.Cmd
	if strings.ContainsAny(ed, "|&;<>()$`\\\"' \t\n*?[#~=%") {
		cmd = exec.Command("sh", "-c", ed+` "$@"`, "$EDITOR", filename)
	} else {
		cmd = exec.Command(ed, filename)
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("invoking editor: %v", err)
	}
	return nil
}

func wrap(t string, prefix string) string {
	out := ""
	t = strings.Replace(t, "\r\n", "\n", -1)
	lines := strings.Split(t, "\n")
	for i, line := range lines {
		if i > 0 {
			out += "\n" + prefix
		}
		s := line
		for len(s) > 70 {
			i := strings.LastIndex(s[:70], " ")
			if i < 0 {
				i = 69
			}
			i++
			out += s[:i] + "\n" + prefix
			s = s[i:]
		}
		out += s
	}
	return out
}

var client *github.Client

// GitHub personal access token, from https://github.com/settings/applications.
var authToken string

func loadAuth() {
	const short = ".github-issue-token"
	filename := filepath.Clean(os.Getenv("HOME") + "/" + short)
	shortFilename := filepath.Clean("$HOME/" + short)
	if *tokenFile != "" {
		filename = *tokenFile
		shortFilename = *tokenFile
	}
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal("reading token: ", err, "\n\n"+
			"Please create a personal access token at https://github.com/settings/tokens/new\n"+
			"and write it to ", shortFilename, " to use this program.\n"+
			"The token only needs the repo scope, or private_repo if you want to\n"+
			"view or edit issues for private repositories.\n"+
			"The benefit of using a personal access token over using your GitHub\n"+
			"password directly is that you can limit its use and revoke it at any time.\n\n")
	}
	fi, err := os.Stat(filename)
	if fi.Mode()&0077 != 0 {
		log.Fatalf("reading token: %s mode is %#o, want %#o", shortFilename, fi.Mode()&0777, fi.Mode()&0700)
	}
	authToken = strings.TrimSpace(string(data))
	t := &oauth2.Transport{
		Source: &tokenSource{AccessToken: authToken},
	}
	client = github.NewClient(&http.Client{Transport: t})
}

type tokenSource oauth2.Token

func (t *tokenSource) Token() (*oauth2.Token, error) {
	return (*oauth2.Token)(t), nil
}

func getInt(x *int) int {
	if x == nil {
		return 0
	}
	return *x
}

func getString(x *string) string {
	if x == nil {
		return ""
	}
	return *x
}

func getUserLogin(x *github.User) string {
	if x == nil || x.Login == nil {
		return ""
	}
	return *x.Login
}

func getTime(x *time.Time) time.Time {
	if x == nil {
		return time.Time{}
	}
	return *x
}

var commitStart = regexp.MustCompile(`^commit (.*)$`)
var diffStart = `diff --git `
var fileStart = regexp.MustCompile(`^\+\+\+ b\/(.*)$`)
var hunkStart = `@@`

func parseFile(b []byte) (*github.PullRequestReviewRequest, error) {
	dat := string(b)

	commit := ""
	file := ""
	num := 0
	foundFirstHunk := false

	commentStart := -1
	lastCommentStart := -1

	topLevelCommentStart := 0

	review := &github.PullRequestReviewRequest{}

	off := 0
	for _, line := range strings.SplitAfter(dat, "\n") {
		lastCommentStart = commentStart
		commentStart = -1
		if line == "" {
			break
		}

		off += len(line)
		line = strings.TrimRight(line, "\n")

		// Process top level comments.
		if line == topLevelStartMarker {
			topLevelCommentStart = off
			continue
		} else if line == topLevelEndMarker {
			body := string(dat[topLevelCommentStart : off-len(line)-2])
			review.Body = &body
			topLevelCommentStart = 0
			continue
		} else if topLevelCommentStart != 0 {
			continue
		}

		// Process commit header.
		commitMatches := commitStart.FindStringSubmatch(line)
		if len(commitMatches) > 1 {
			foundFirstHunk = false
			commit = commitMatches[1]
			review.CommitID = &commit
			continue
		}

		// Process diff header. This means we're in a diff until wee see another
		// diff or commit marker.
		if strings.HasPrefix(line, diffStart) {
			foundFirstHunk = false
			continue
		}

		// Process file header.
		fileMatches := fileStart.FindStringSubmatch(line)
		if len(fileMatches) > 1 {
			file = fileMatches[1]
			continue
		}
		// Process first hunk header.
		if !foundFirstHunk {
			if strings.HasPrefix(line, hunkStart) {
				foundFirstHunk = true
				num = 0
			}
			continue
		}

		// Process special diff first-chars.
		if len(line) > 0 {
			switch line[0] {
			case '+', '-', ' ', '@':
				num++
				continue
			case '*':
				// Old comment
				continue
			}
		}
		// We found a comment!
		commentStart = lastCommentStart
		if commentStart == -1 {
			commentStart = off - len(line) - 1
			review.Comments = append(review.Comments,
				makeDraftReviewComment(file, num))
		}
		c := review.Comments[len(review.Comments)-1]
		body := dat[commentStart : off-1]
		c.Body = &body
	}

	return review, nil
}

func makeDraftReviewComment(path string, position int) *github.DraftReviewComment {
	return &github.DraftReviewComment{
		Path:     &path,
		Position: &position,
	}
}

func postComments(ctx context.Context, pr int, review *github.PullRequestReviewRequest) error {
	log.Printf("Submitting %d comments...\n", len(review.Comments))
	fmt.Println(review)
	if len(review.Comments) > 0 {
		_, _, err := client.PullRequests.CreateReview(ctx, projectOwner, projectRepo, pr, review)
		if err != nil {
			return err
		}
	}
	return nil
}
