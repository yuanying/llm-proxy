package login

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
	"github.com/yuanying/llm-proxy/internal/oauth"
)

// RunXAI signs in via device authorization and atomically replaces auth.json.
func RunXAI(ctx context.Context, client *oauth.XAIClient, clientID string, store *auth.FileStore, out io.Writer) error {
	cred, err := client.DeviceLogin(ctx, clientID, func(uri, code string) {
		_, _ = fmt.Fprintf(out, "Open this URL in a browser and sign in with SuperGrok or X Premium:\n\n%s\n\nEnter this code: %s\n", uri, code)
	})
	if err != nil {
		return err
	}
	if _, err := store.Save(cred); err != nil {
		return fmt.Errorf("writing %s: %w", store.Path(), err)
	}
	_, _ = fmt.Fprintf(out, "Saved the credential to %s. It expires at %s and is refreshed by `llm-proxy serve`.\n",
		store.Path(), cred.ExpiresAt.Format(time.RFC3339))
	return nil
}
