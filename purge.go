package main

import (
	"context"
	"time"
)

// StartPurge runs the expiry sweep on a ticker inside this process.
//
// ponytail: a goroutine, not a job queue. The work is one indexed query an hour against a
// SQLite file; Redis and a worker container would be infrastructure carrying nothing.
func (a *App) StartPurge(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		a.PurgeExpired(ctx) // once at boot, so a long outage does not wait an hour
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.PurgeExpired(ctx)
			}
		}
	}()
}

// PurgeExpired deletes objects whose time is up, then trims the audit trail.
//
// A row whose file fails to delete is left alone so the next sweep retries it - the volume
// erroring must not silently orphan bytes we have stopped tracking.
func (a *App) PurgeExpired(ctx context.Context) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT id FROM uploads WHERE expires_at <= ? LIMIT 500`, time.Now().Unix())
	if err != nil {
		a.Log.Printf("purge: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			a.Log.Printf("purge: reading the expiry list: %v", err)
			break
		}
		ids = append(ids, id)
	}
	// A sweep that stopped early is not a sweep that found nothing; say so rather than
	// leaving expired files on the volume with a silent log.
	if err := rows.Err(); err != nil {
		a.Log.Printf("purge: the expiry list ended early: %v", err)
	}
	rows.Close()

	purged := 0
	for _, id := range ids {
		u, err := a.FindUploadByID(ctx, id)
		if err != nil {
			continue
		}
		if err := a.Purge(ctx, u, "purged", false); err != nil {
			a.Log.Printf("purge %s: %v", u.ObjectKey, err)
			continue
		}
		purged++
	}

	cutoff := time.Now().Add(-EventRetention).Unix()
	if _, err := a.DB.ExecContext(ctx, `DELETE FROM upload_events WHERE created_at < ?`, cutoff); err != nil {
		a.Log.Printf("trimming upload_events: %v", err)
	}
	if _, err := a.DB.ExecContext(ctx, `DELETE FROM counters WHERE expires_at < ?`, time.Now().Unix()); err != nil {
		a.Log.Printf("trimming counters: %v", err)
	}
	if purged > 0 {
		a.Log.Printf("purged %d expired uploads", purged)
	}
}
