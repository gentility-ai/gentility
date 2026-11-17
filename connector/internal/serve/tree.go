package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/tree"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// treeAttendance shares the running overlay hop. It owns no overlay session,
// and a tree refusal cannot stop the connector's config or presence loops.
type treeAttendance struct {
	sourceMu        sync.RWMutex
	source          tree.Source
	once            sync.Once
	done            chan struct{}
	writeDiscovery  func(string, tree.Discovery, tree.Trust) error
	discoveryNotice string
}

func (t *treeAttendance) apply(ctx context.Context, a Attendance, document *config.Document, base string, client *http.Client, logf func(string, ...any)) (applyErr error) {
	notice := ""
	defer func() {
		if applyErr != nil {
			notice = fmt.Sprintf("%v; retrying on the next config poll", applyErr)
		}
		if notice != t.discoveryNotice {
			if notice == "" {
				logf("tree discovery: previous refusal cleared")
			} else {
				logf("%s", notice)
			}
			t.discoveryNotice = notice
		}
	}()
	address, err := netip.ParseAddr(document.Gateway.Address)
	org, orgID := "", ""
	if err == nil {
		grants, e := tsnode.ParseGrants(document.Grants)
		if e == nil {
			if grant, e := tsnode.SourceFor(grants, address); e == nil {
				org, orgID = grant.Org, grant.OrgID
			}
		}
	}
	t.sourceMu.Lock()
	t.source = tree.Source{BaseURL: base, HTTP: client, Bearer: a.Identity.NodeCredential, Organization: org, OrganizationID: orgID}
	t.sourceMu.Unlock()
	identity, err := a.Store.LoadNode()
	if err != nil {
		return err
	}
	directory := tree.DiscoveryDirectory(a.Store.Dir)
	if org != "" && document.Gateway.Port > 0 && identity.Origin != "" {
		discovery := tree.Discovery{
			Version: 1, Origin: identity.Origin,
			Gateway: netip.AddrPortFrom(address, uint16(document.Gateway.Port)).String(),
			CAPEM:   document.Gateway.CAPEM, Organization: org, OrganizationID: orgID,
			Interface: a.Session.Node.Interface(),
		}
		write := t.writeDiscovery
		if write == nil {
			write = tree.WriteDiscovery
		}
		if err := write(directory, discovery, nil); err != nil {
			return fmt.Errorf("tree discovery: %w", err)
		}
	}
	if org != "" && document.Gateway.Port > 0 && identity.Origin == "" {
		notice = fmt.Sprintf("tree discovery skipped: node %s has no login origin yet", identity.NodeID)
	}
	// A config without a resolved organization cannot establish a tree binding.
	// A later attendance update will start the loop once the grant is available.
	if org == "" {
		return nil
	}
	t.once.Do(func() {
		t.done = make(chan struct{})
		go func() {
			defer close(t.done)
			options := tree.Options{
				Root:      filepath.Join(directory, "tree"),
				StateDir:  filepath.Join(directory, "tree-sync"),
				Principal: "node", Log: logf,
				Source: func(context.Context) (tree.Source, error) {
					t.sourceMu.RLock()
					defer t.sourceMu.RUnlock()
					return t.source, nil
				},
			}
			logf("tree: group %s grants read access to %s; add operators to that group", tree.DaemonGroup(runtime.GOOS), options.Root)
			if err := tree.Run(ctx, options); err != nil {
				logf("tree stopped: %v", err)
			}
		}()
	})
	return nil
}
func (t *treeAttendance) wait() {
	if t.done != nil {
		<-t.done
	}
}
