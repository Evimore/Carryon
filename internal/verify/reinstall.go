package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/record"
)

// checkReinstall checks that everything in the record can be installed again: the repository
// packages are still offered, the AUR packages still exist, the extra repositories answer and
// their keys are saved.
func (v *verifier) checkReinstall(ctx context.Context) {
	r := v.rep
	r.section("Can it be reinstalled?")
	var pkgs, aur []string
	for _, it := range v.rec.Items {
		switch it.Type {
		case record.TypePkg:
			pkgs = append(pkgs, it.ID)
		case record.TypeAUR:
			aur = append(aur, it.ID)
		}
	}

	sync, err := v.opt.System.Packages.SyncPackages(ctx)
	if err != nil {
		r.add(Line{Level: Warning, Text: "The repositories' package lists couldn't be read: " + err.Error(),
			Fix: "Refresh them with: sudo pacman -Sy"})
	} else {
		offered := map[string]bool{}
		for _, p := range sync {
			offered[p] = true
		}
		var gone []string
		for _, p := range pkgs {
			if !offered[p] {
				gone = append(gone, p)
			}
		}
		r.check(Problem, gone,
			fmt.Sprintf("All %d repository packages are still available.", len(pkgs)),
			fmt.Sprintf("%d of %d repository packages are no longer in the repositories:", len(gone), len(pkgs)),
			"A package may have been renamed or dropped: install its replacement, or remove it with sudo pacman -R.")
		if len(aur) > 0 {
			v.checkHelper(pkgs, aur, offered)
		}
	}
	if len(aur) > 0 {
		v.checkAUR(ctx, aur)
	}
	for _, it := range v.rec.Items {
		if it.Type == record.TypeRepo {
			v.checkRepo(ctx, it)
		}
	}
}

// aurHelpers can rebuild AUR packages, in the order restore prefers them.
var aurHelpers = []string{"paru", "yay"}

// checkHelper checks that an AUR helper can be had on a new machine.
func (v *verifier) checkHelper(pkgs, aur []string, offered map[string]bool) {
	for _, h := range aurHelpers {
		if slices.Contains(pkgs, h) || slices.Contains(aur, h) {
			v.rep.ok("The AUR helper %s is in the package list.", h)
			return
		}
	}
	for _, h := range aurHelpers {
		if offered[h] {
			v.rep.ok("No AUR helper is in the package list, but %s is in the repositories, so restore can install it first.", h)
			return
		}
	}
	v.rep.add(Line{Level: Warning, Text: "No AUR helper (paru or yay) is in the package list, so a new machine needs one built by hand before the AUR packages.",
		Fix: "Install paru or yay here, so the next backup records it."})
}

// aurBatch is how many packages one AUR request asks about, keeping the URL short.
const aurBatch = 100

// aurPackage is one result of the AUR's info request.
type aurPackage struct {
	Name       string  `json:"Name"`
	Maintainer *string `json:"Maintainer"`
	OutOfDate  *int64  `json:"OutOfDate"`
}

// aurInfo asks the AUR about packages, through its RPC API (version 5). Ported from aur_info.
func (v *verifier) aurInfo(ctx context.Context, names []string) (map[string]aurPackage, error) {
	out := map[string]aurPackage{}
	for start := 0; start < len(names); start += aurBatch {
		batch := names[start:min(start+aurBatch, len(names))]
		q := url.Values{}
		for _, n := range batch {
			q.Add("arg[]", n)
		}
		u := strings.TrimRight(v.opt.AURBase, "/") + "/rpc/v5/info?" + q.Encode()
		body, err := v.get(ctx, u)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Type    string       `json:"type"`
			Error   string       `json:"error"`
			Results []aurPackage `json:"results"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("reading the AUR's answer: %w", err)
		}
		if resp.Type == "error" {
			return nil, fmt.Errorf("the AUR answered: %s", resp.Error)
		}
		for _, p := range resp.Results {
			out[p.Name] = p
		}
	}
	return out, nil
}

// maxBody is the most of a web answer read.
const maxBody = 16 << 20

// get fetches a URL and returns its body.
func (v *verifier) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.opt.HTTP.Do(req)
	if err != nil {
		return nil, &offlineError{err}
	}
	defer resp.Body.Close()
	v.netSeen = true
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// offlineError is a request that got no answer at all: no network, or the server is down.
type offlineError struct{ err error }

func (e *offlineError) Error() string { return e.err.Error() }
func (e *offlineError) Unwrap() error { return e.err }

// checkAUR checks that the AUR packages still exist there, and notes the orphaned and out-of-date
// ones. When the AUR can't be reached, that's a warning: the packages may well be there.
func (v *verifier) checkAUR(ctx context.Context, aur []string) {
	info, err := v.aurInfo(ctx, aur)
	if err != nil {
		v.rep.add(Line{Level: Warning, Text: "The AUR couldn't be reached to check " + record.Brief(aur, 4) + ": " + err.Error(),
			Fix: "This is fine when offline; verify checks again next time."})
		return
	}
	var missing, notes []string
	for _, p := range aur {
		got, ok := info[p]
		switch {
		case !ok:
			missing = append(missing, p)
		case got.Maintainer == nil:
			notes = append(notes, p+": orphaned (nobody maintains it)")
		case got.OutOfDate != nil:
			notes = append(notes, p+": flagged out of date")
		}
	}
	v.rep.check(Problem, missing,
		fmt.Sprintf("All %d AUR packages still exist on the AUR.", len(aur)),
		fmt.Sprintf("%d of %d AUR packages are no longer on the AUR:", len(missing), len(aur)),
		"A new machine can't rebuild these: find a replacement, or keep the package's build files somewhere backed up.")
	if len(notes) > 0 {
		v.rep.add(Line{Level: Info, Text: "AUR notes: " + record.Brief(notes, 4)})
	}
}

// serverLine matches a Server line in a pacman.conf section.
var (
	serverLine  = regexp.MustCompile(`(?m)^\s*Server\s*=\s*(\S+)`)
	includeLine = regexp.MustCompile(`(?m)^\s*Include\s*=\s*(\S+)`)
)

// checkRepo checks that an extra repository answers and that its signing key is saved.
func (v *verifier) checkRepo(ctx context.Context, it record.Item) {
	ok, where, offline := v.repoReachable(ctx, it.ID, it.Section)
	switch {
	case ok:
		v.rep.ok("The extra repository [%s] answers.", it.ID)
	case offline && !v.netSeen:
		v.rep.add(Line{Level: Warning, Text: fmt.Sprintf("The extra repository [%s] couldn't be reached: %s", it.ID, where),
			Fix: "This is fine when offline; verify checks again next time."})
	default:
		v.rep.add(Line{Level: Problem, Text: fmt.Sprintf("The extra repository [%s] doesn't answer: %s", it.ID, where),
			Fix: "A new machine couldn't install its packages: check its Server lines in " + repoConf(it) + "."})
	}
	keys, err := os.Stat(filepath.Join(v.opt.Dirs.RecordDir(), capture.RepoKeysFile))
	if len(it.Keys) > 0 && err == nil && keys.Size() > 0 {
		v.rep.ok("[%s]'s signing key is saved.", it.ID)
		return
	}
	v.rep.add(Line{Level: Warning, Text: fmt.Sprintf("No signing key added by hand was found for [%s].", it.ID),
		Fix: "If pacman needs a key for it, add the key with pacman-key; the next backup saves it."})
}

func repoConf(it record.Item) string {
	if it.Conf != "" {
		return it.Conf
	}
	return "/etc/pacman.conf"
}

// repoReachable tries each Server of a repository, also those in files its section includes, until
// one has the repository's database. offline means no server answered at all. Ported from
// repo_reachable.
func (v *verifier) repoReachable(ctx context.Context, repo, section string) (ok bool, where string, offline bool) {
	var servers []string
	for _, m := range serverLine.FindAllStringSubmatch(section, -1) {
		servers = append(servers, m[1])
	}
	for _, m := range includeLine.FindAllStringSubmatch(section, -1) {
		data, err := os.ReadFile(filepath.Join(v.root(), m[1]))
		if err != nil {
			continue
		}
		for _, s := range serverLine.FindAllStringSubmatch(string(data), -1) {
			servers = append(servers, s[1])
		}
	}
	if len(servers) == 0 {
		return false, "its section has no Server line", false
	}
	offline = true
	where = ""
	for _, s := range servers {
		u := strings.NewReplacer("$repo", repo, "$arch", v.opt.Arch).Replace(s)
		u = strings.TrimRight(u, "/") + "/" + repo + ".db"
		var err error
		if local, ok := strings.CutPrefix(u, "file://"); ok {
			_, err = os.Stat(filepath.Join(v.root(), local))
		} else {
			err = v.head(ctx, u)
		}
		if err == nil {
			return true, u, false
		}
		var off *offlineError
		if !errors.As(err, &off) {
			offline = false
		}
		where = u + ": " + err.Error()
	}
	return false, where, offline
}

// head checks that a URL answers a HEAD request with success.
func (v *verifier) head(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return err
	}
	resp, err := v.opt.HTTP.Do(req)
	if err != nil {
		return &offlineError{err}
	}
	resp.Body.Close()
	v.netSeen = true
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}
