package stack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
	"golang.org/x/sync/errgroup"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/host"
	"github.com/luigi23997/azemu/internal/infra"
)

// CreateObjects creates the declared containers, tables and queues in Azurite, one goroutine per account.
// Existing objects are success. Talks to the published Azurite ports on 127.0.0.1 over TLS trusting caPEM.
func CreateObjects(ctx context.Context, m *infra.Model, caPEM []byte) error {
	return createObjects(ctx, m, caPEM, "127.0.0.1", contract.AzuriteBlobPort, contract.AzuriteQueuePort, contract.AzuriteTablePort)
}

func createObjects(ctx context.Context, m *infra.Model, caPEM []byte, addrHost string, blobPort, queuePort, tablePort int) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("storage: no certificate found in CA PEM")
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: host.DialContext, TLSClientConfig: &tls.Config{RootCAs: pool}}}
	opts := azcore.ClientOptions{Transport: hc}
	url := func(port int, acct string) string { return fmt.Sprintf("https://%s:%d/%s", addrHost, port, acct) }

	g, ctx := errgroup.WithContext(ctx)
	for _, a := range m.StorageAccounts {
		g.Go(func() error {
			if err := createAccount(ctx, a, opts, url(blobPort, a.Name), url(queuePort, a.Name), url(tablePort, a.Name)); err != nil {
				return fmt.Errorf("storage account %s: %w", a.Name, err)
			}
			return nil
		})
	}
	return g.Wait()
}

func createAccount(ctx context.Context, a infra.StorageAccount, opts azcore.ClientOptions, blobURL, queueURL, tableURL string) error {
	bc, err := azblob.NewSharedKeyCredential(a.Name, contract.AzuriteKey)
	if err != nil {
		return err
	}
	blob, err := azblob.NewClientWithSharedKeyCredential(blobURL, bc, &azblob.ClientOptions{ClientOptions: opts})
	if err != nil {
		return err
	}
	qc, _ := azqueue.NewSharedKeyCredential(a.Name, contract.AzuriteKey)
	queue, err := azqueue.NewServiceClientWithSharedKeyCredential(queueURL, qc, &azqueue.ClientOptions{ClientOptions: opts})
	if err != nil {
		return err
	}
	tc, _ := aztables.NewSharedKeyCredential(a.Name, contract.AzuriteKey)
	table, err := aztables.NewServiceClientWithSharedKey(tableURL, tc, &aztables.ClientOptions{ClientOptions: opts})
	if err != nil {
		return err
	}
	for _, n := range a.Containers {
		_, err = blob.CreateContainer(ctx, n, nil)
		if err = IgnoreExists(err); err != nil {
			return fmt.Errorf("container %s: %w", n, err)
		}
	}
	for _, n := range a.Queues {
		_, err = queue.CreateQueue(ctx, n, nil)
		if err = IgnoreExists(err); err != nil {
			return fmt.Errorf("queue %s: %w", n, err)
		}
	}
	for _, n := range a.Tables {
		_, err = table.CreateTable(ctx, n, nil)
		if err = IgnoreExists(err); err != nil {
			return fmt.Errorf("table %s: %w", n, err)
		}
	}
	return nil
}

// IgnoreExists maps HTTP 409 (ContainerAlreadyExists, TableAlreadyExists, QueueAlreadyExists) to nil.
func IgnoreExists(err error) error {
	var re *azcore.ResponseError
	if errors.As(err, &re) && re.StatusCode == http.StatusConflict {
		return nil
	}
	return err
}
