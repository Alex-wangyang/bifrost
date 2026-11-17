//go:build cgo

package oauth2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type lighthouseContendedStore struct {
	*testConfigStore
	writes   int
	failures int
}

func (s *lighthouseContendedStore) RefreshOauthTokenFieldsIfActive(ctx context.Context, id, prior, access, refresh string, exp *time.Time, at time.Time) (bool, error) {
	s.writes++
	if s.writes <= s.failures {
		return false, fmt.Errorf("wrapped: %w", sqlite3.Error{Code: sqlite3.ErrBusy})
	}
	return s.testConfigStore.RefreshOauthTokenFieldsIfActive(ctx, id, prior, access, refresh, exp, at)
}
func TestLighthouseRefreshRetriesWriteNotExchange(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer server.Close()
	base := newTestConfigStore()
	_, id := seedFixtures(t, base, server.URL+"/token")
	store := &lighthouseContendedStore{testConfigStore: base, failures: 2}
	p := NewOAuth2Provider(store, bifrost.NewDefaultLogger(schemas.LogLevelError))
	require.NoError(t, p.RefreshAccessToken(context.Background(), id))
	require.Equal(t, 3, store.writes)
	require.EqualValues(t, 1, requests.Load())
	token, err := base.GetOauthTokenByID(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "rotated", token.RefreshToken)
}
func TestLighthousePersistenceRetryBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		updated bool
		want    int
	}{{"permanent", errors.New("disk full"), false, 1}, {"lost CAS", nil, false, 1}, {"success", nil, true, 1}, {"busy exhaustion", sqlite3.Error{Code: sqlite3.ErrBusy}, false, 3}, {"locked exhaustion", sqlite3.Error{Code: sqlite3.ErrLocked}, false, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			_, err := retryOAuthPersistence(context.Background(), func() (bool, error) { n++; return tc.updated, tc.err })
			require.Equal(t, tc.want, n)
			if tc.err != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	_, err := retryOAuthPersistence(ctx, func() (bool, error) { n++; cancel(); return false, sqlite3.Error{Code: sqlite3.ErrBusy} })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, n)
}

type lighthouseRealStore struct {
	configstore.ConfigStore
	writes     int
	release    func()
	lockErrors int
}

func (s *lighthouseRealStore) RefreshOauthTokenFieldsIfActive(ctx context.Context, id, prior, access, refresh string, exp *time.Time, at time.Time) (bool, error) {
	s.writes++
	ok, err := s.ConfigStore.RefreshOauthTokenFieldsIfActive(ctx, id, prior, access, refresh, exp, at)
	if isLighthouseSQLiteLock(err) {
		s.lockErrors++
		if s.release != nil {
			s.release()
			s.release = nil
		}
	}
	return ok, err
}
func TestLighthouseRealSQLiteRefresh(t *testing.T) {
	for _, mode := range []string{"native-busy-wait", "retry-after-lock", "exhaustion", "cancelled-write"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			log := bifrost.NewDefaultLogger(schemas.LogLevelError)
			encrypt.Init("rc2-fixture-encryption-key", log)
			defer encrypt.Init("", log)
			path := filepath.Join(t.TempDir(), "oauth.db")
			store, err := configstore.NewConfigStore(ctx, &configstore.Config{Enabled: true, Type: configstore.ConfigStoreTypeSQLite, Config: &configstore.SQLiteConfig{Path: path}}, log)
			require.NoError(t, err)
			defer store.Close(ctx)
			db, err := store.DB().DB()
			require.NoError(t, err)
			db.SetMaxOpenConns(1)
			var busy int
			require.NoError(t, db.QueryRow("PRAGMA busy_timeout").Scan(&busy))
			require.Equal(t, 60000, busy)
			var exchanges atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"rc2-new","refresh_token":"rc2-rotated","expires_in":3600}`)
			}))
			defer srv.Close()
			fixtures := newTestConfigStore()
			cid, tid := seedFixtures(t, fixtures, srv.URL+"/token")
			require.NoError(t, store.CreateOauthConfig(ctx, fixtures.oauthConfigs[cid]))
			require.NoError(t, store.CreateOauthToken(ctx, fixtures.oauthTokens[tid]))
			locker, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_txlock=immediate")
			require.NoError(t, err)
			defer locker.Close()
			tx, err := locker.Begin()
			require.NoError(t, err)
			defer tx.Rollback()
			if mode == "cancelled-write" {
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				released := make(chan struct{})
				go func() {
					time.Sleep(30 * time.Millisecond)
					cancel()
					time.Sleep(30 * time.Millisecond)
					_ = tx.Rollback()
					close(released)
				}()
				calls := 0
				_, err := retryOAuthPersistence(cancelCtx, func() (bool, error) {
					calls++
					return store.RefreshOauthTokenFieldsIfActive(cancelCtx, tid, "refresh-token", "bad", "bad", nil, time.Now())
				})
				<-released
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
				token, err := store.GetOauthTokenByID(ctx, tid)
				require.NoError(t, err)
				require.Equal(t, "refresh-token", token.RefreshToken)
				return
			}
			wrapped := &lighthouseRealStore{ConfigStore: store}
			if mode == "native-busy-wait" {
				go func() { time.Sleep(60 * time.Millisecond); _ = tx.Rollback() }()
			} else {
				_, err = db.Exec("PRAGMA busy_timeout=1")
				require.NoError(t, err)
				if mode == "retry-after-lock" {
					wrapped.release = func() { require.NoError(t, tx.Rollback()) }
				}
			}
			provider := NewOAuth2Provider(wrapped, log)
			err = provider.RefreshAccessToken(ctx, tid)
			require.EqualValues(t, 1, exchanges.Load())
			if mode == "exhaustion" {
				require.Error(t, err)
				require.Equal(t, 3, wrapped.writes)
				require.Equal(t, 3, wrapped.lockErrors)
				require.NoError(t, tx.Rollback())
				token, e := store.GetOauthTokenByID(ctx, tid)
				require.NoError(t, e)
				require.Equal(t, "refresh-token", token.RefreshToken)
				return
			}
			require.NoError(t, err)
			if mode == "retry-after-lock" {
				require.Equal(t, 2, wrapped.writes)
				require.Equal(t, 1, wrapped.lockErrors)
			} else {
				require.Equal(t, 1, wrapped.writes)
			}
			token, err := store.GetOauthTokenByID(ctx, tid)
			require.NoError(t, err)
			require.Equal(t, "rc2-rotated", token.RefreshToken)
			var persisted string
			require.NoError(t, db.QueryRow("SELECT refresh_token FROM mcp_oauth_tokens WHERE id=?", tid).Scan(&persisted))
			require.NotEqual(t, "rc2-rotated", persisted)
			calls := 0
			updated, err := retryOAuthPersistence(ctx, func() (bool, error) {
				calls++
				return store.RefreshOauthTokenFieldsIfActive(ctx, tid, "refresh-token", "stale", "stale", nil, time.Now())
			})
			require.NoError(t, err)
			require.False(t, updated)
			require.Equal(t, 1, calls)
		})
	}
}
