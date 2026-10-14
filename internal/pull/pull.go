// Package pull copies a selected slice of real Azure Storage data (blobs, table entities) into the
// local emulated account. The source is only ever read; the destination is one Azurite account.
package pull

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"golang.org/x/sync/errgroup"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/stack"
)

// Clients is one storage account as the copy sees it (queues are never copied).
type Clients struct {
	Blob  *azblob.Client
	Table *aztables.ServiceClient
}

type Options struct {
	Containers, Tables []string
	All                bool   // every container and every table of the source
	Prefix             string // blob name prefix, applies to every selected container
	Concurrency        int    // parallel blob/entity transfers
	Secret             string // scrubbed from every error text (the source account key)
}

// Validate checks the selection flags.
func (o *Options) Validate() error {
	switch {
	case o.All && (len(o.Containers) > 0 || len(o.Tables) > 0):
		return errors.New("--all cannot be combined with --containers or --tables")
	case !o.All && len(o.Containers) == 0 && len(o.Tables) == 0:
		return errors.New("nothing selected: pass --containers, --tables or --all")
	case o.Prefix != "" && !o.All && len(o.Containers) == 0:
		return errors.New("--prefix applies to containers: pass --containers or --all")
	case o.Concurrency < 1:
		return errors.New("--concurrency must be at least 1")
	}
	return nil
}

// Stat is the outcome for one container or table. Items counts everything listed (copied, skipped and failed).
type Stat struct {
	Kind, Name string
	Items      int
	Skipped    int
	Failed     int
	Bytes      int64
	Dur        time.Duration
	Errs       []string

	mu     sync.Mutex
	redact func(string) string
}

func (s *Stat) add(items, skipped, failed int, bytes int64) {
	s.mu.Lock()
	s.Items, s.Skipped, s.Failed, s.Bytes = s.Items+items, s.Skipped+skipped, s.Failed+failed, s.Bytes+bytes
	s.mu.Unlock()
}

func (s *Stat) fail(item string, err error) {
	s.mu.Lock()
	s.Failed++
	s.Items++
	if len(s.Errs) < 20 { // ponytail: first 20 errors per unit are kept, the rest only counted
		s.Errs = append(s.Errs, s.redact(strings.TrimSpace(item+": "+err.Error())))
	}
	s.mu.Unlock()
}

// Run copies the selection from src to dst, printing one line per container/table and a total to out.
// The returned error is non-nil if anything failed; every Stat is returned either way.
func Run(ctx context.Context, src, dst Clients, o Options, out io.Writer) ([]*Stat, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	redact := func(s string) string {
		if o.Secret != "" {
			s = strings.ReplaceAll(s, o.Secret, "***")
		}
		return s
	}
	conts, tabs := slices.Clone(o.Containers), slices.Clone(o.Tables)
	if o.All {
		var err error
		if conts, tabs, err = listAll(ctx, src); err != nil {
			return nil, errors.New(redact(err.Error()))
		}
	}

	var stats []*Stat
	for _, c := range conts {
		stats = append(stats, copyContainer(ctx, src.Blob, dst.Blob, c, o, redact))
	}
	for _, t := range tabs {
		stats = append(stats, copyTable(ctx, src.Table, dst.Table, t, o, redact))
	}

	var tot Stat
	for _, s := range stats {
		fmt.Fprintf(out, "%-9s %s: %s\n", s.Kind, s.Name, s.summary())
		tot.Items, tot.Skipped, tot.Failed, tot.Bytes, tot.Dur = tot.Items+s.Items, tot.Skipped+s.Skipped, tot.Failed+s.Failed, tot.Bytes+s.Bytes, tot.Dur+s.Dur
	}
	fmt.Fprintf(out, "total: %s\n", tot.summary())
	for _, s := range stats {
		for _, e := range s.Errs {
			fmt.Fprintf(out, "error: %s %s: %s\n", s.Kind, s.Name, e)
		}
	}
	if tot.Failed > 0 {
		return stats, fmt.Errorf("%d item(s) failed", tot.Failed)
	}
	return stats, ctx.Err()
}

func (s *Stat) summary() string {
	return fmt.Sprintf("%d items, %d bytes, %d skipped, %d failed, %.1fs", s.Items, s.Bytes, s.Skipped, s.Failed, s.Dur.Seconds())
}

func listAll(ctx context.Context, src Clients) (conts, tabs []string, err error) {
	cp := src.Blob.NewListContainersPager(nil)
	for cp.More() {
		p, err := cp.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list containers: %w", err)
		}
		for _, c := range p.ContainerItems {
			conts = append(conts, *c.Name)
		}
	}
	tp := src.Table.NewListTablesPager(nil)
	for tp.More() {
		p, err := tp.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list tables: %w", err)
		}
		for _, t := range p.Tables {
			tabs = append(tabs, *t.Name)
		}
	}
	return conts, tabs, nil
}

// sameBlob reports whether the destination already holds the source blob: equal size and both
// Content-MD5 present and equal. A missing MD5 on either side means "cannot prove it", so copy.
func sameBlob(srcSize int64, srcMD5 []byte, dstSize int64, dstMD5 []byte) bool {
	return srcSize == dstSize && len(srcMD5) > 0 && bytes.Equal(srcMD5, dstMD5)
}

type countReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func copyContainer(ctx context.Context, src, dst *azblob.Client, name string, o Options, redact func(string) string) *Stat {
	st := &Stat{Kind: "container", Name: name, redact: redact}
	t0 := time.Now()
	defer func() { st.Dur = time.Since(t0) }()
	if _, err := dst.CreateContainer(ctx, name, nil); stack.IgnoreExists(err) != nil {
		st.fail("create local container", err)
		return st
	}
	lo := &azblob.ListBlobsFlatOptions{Include: container.ListBlobsInclude{Metadata: true}}
	if o.Prefix != "" {
		lo.Prefix = &o.Prefix
	}
	var g errgroup.Group
	g.SetLimit(o.Concurrency)
	pager := src.NewListBlobsFlatPager(name, lo)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			st.fail("list blobs", err)
			break
		}
		for _, b := range page.Segment.BlobItems {
			g.Go(func() error {
				copyBlob(ctx, src, dst, name, b, st)
				return nil
			})
		}
	}
	g.Wait()
	return st
}

func copyBlob(ctx context.Context, src, dst *azblob.Client, cont string, b *container.BlobItem, st *Stat) {
	name, p := *b.Name, b.Properties
	var size int64
	if p.ContentLength != nil {
		size = *p.ContentLength
	}
	dp, err := dst.ServiceClient().NewContainerClient(cont).NewBlobClient(name).GetProperties(ctx, nil)
	if err == nil && dp.ContentLength != nil && sameBlob(size, p.ContentMD5, *dp.ContentLength, dp.ContentMD5) {
		st.add(1, 1, 0, 0)
		return
	}
	dl, err := src.DownloadStream(ctx, cont, name, nil)
	if err != nil {
		st.fail(name, err)
		return
	}
	body := dl.NewRetryReader(ctx, nil)
	defer body.Close()
	cr := &countReader{r: body}
	// ponytail: block blobs only (page/append blobs arrive as block blobs); 4 MiB blocks cap a blob at ~195 GiB.
	_, err = dst.UploadStream(ctx, cont, name, cr, &azblob.UploadStreamOptions{
		BlockSize: 4 << 20, Concurrency: 1, Metadata: b.Metadata,
		HTTPHeaders: &blob.HTTPHeaders{
			BlobContentType: p.ContentType, BlobContentEncoding: p.ContentEncoding, BlobCacheControl: p.CacheControl,
			BlobContentDisposition: p.ContentDisposition, BlobContentLanguage: p.ContentLanguage,
			BlobContentMD5: p.ContentMD5, // kept so the next pull can skip this blob
		},
	})
	if err != nil {
		st.fail(name, err)
		return
	}
	st.add(1, 0, 0, cr.n.Load())
}

func copyTable(ctx context.Context, src, dst *aztables.ServiceClient, name string, o Options, redact func(string) string) *Stat {
	st := &Stat{Kind: "table", Name: name, redact: redact}
	t0 := time.Now()
	defer func() { st.Dur = time.Since(t0) }()
	if _, err := dst.CreateTable(ctx, name, nil); stack.IgnoreExists(err) != nil {
		st.fail("create local table", err)
		return st
	}
	dc := dst.NewClient(name)
	var g errgroup.Group
	g.SetLimit(o.Concurrency)
	// ponytail: one upsert request per entity; entity-group batches (100 per partition) if big tables get slow.
	pager := src.NewClient(name).NewListEntitiesPager(&aztables.ListEntitiesOptions{Format: to(aztables.MetadataFormatFull)})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			st.fail("list entities", err)
			break
		}
		for _, raw := range page.Entities {
			g.Go(func() error {
				e, key, err := stripEntity(raw)
				if err == nil {
					_, err = dc.UpsertEntity(ctx, e, &aztables.UpsertEntityOptions{UpdateMode: aztables.UpdateModeReplace})
				}
				if err != nil {
					st.fail(key, err)
					return nil
				}
				st.add(1, 0, 0, int64(len(raw)))
				return nil
			})
		}
	}
	g.Wait()
	return st
}

func to[T any](v T) *T { return &v }

// stripEntity removes the server-owned keys (odata.*, Timestamp) from a full-metadata entity and keeps
// the "<prop>@odata.type" annotations, so Int64, DateTime, Guid and Binary survive the upsert.
// Numbers are kept as written (json.Number), so doubles are not reformatted. key is "pk/rk" for errors.
func stripEntity(raw []byte) (out []byte, key string, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err = dec.Decode(&m); err != nil {
		return nil, "?", err
	}
	key = fmt.Sprintf("%v/%v", m["PartitionKey"], m["RowKey"])
	for k := range m {
		if strings.HasPrefix(k, "odata.") || k == "Timestamp" || k == "Timestamp@odata.type" {
			delete(m, k)
		}
	}
	out, err = json.Marshal(m)
	return out, key, err
}

// SourceClients opens the source account from a storage connection string (kept in memory only).
func SourceClients(connStr string) (Clients, error) {
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: host.DialContext, ForceAttemptHTTP2: true}}
	co := azcore.ClientOptions{Transport: hc}
	b, err := azblob.NewClientFromConnectionString(connStr, &azblob.ClientOptions{ClientOptions: co})
	if err != nil {
		return Clients{}, err
	}
	t, err := aztables.NewServiceClientFromConnectionString(connStr, &aztables.ClientOptions{ClientOptions: co})
	return Clients{b, t}, err
}

// LocalClients opens the emulated account acct on Azurite at host (path-style https, SharedKey with the
// well-known Azurite key), trusting caPEM.
func LocalClients(acct string, caPEM []byte, addrHost string, blobPort, tablePort int) (Clients, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return Clients{}, errors.New("no certificate found in CA PEM")
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: host.DialContext, TLSClientConfig: &tls.Config{RootCAs: pool}}}
	co := azcore.ClientOptions{Transport: hc}
	url := func(port int) string { return fmt.Sprintf("https://%s:%d/%s", addrHost, port, acct) }
	bc, err := azblob.NewSharedKeyCredential(acct, contract.AzuriteKey)
	if err != nil {
		return Clients{}, err
	}
	b, err := azblob.NewClientWithSharedKeyCredential(url(blobPort), bc, &azblob.ClientOptions{ClientOptions: co})
	if err != nil {
		return Clients{}, err
	}
	tc, err := aztables.NewSharedKeyCredential(acct, contract.AzuriteKey)
	if err != nil {
		return Clients{}, err
	}
	t, err := aztables.NewServiceClientWithSharedKey(url(tablePort), tc, &aztables.ClientOptions{ClientOptions: co})
	return Clients{b, t}, err
}
