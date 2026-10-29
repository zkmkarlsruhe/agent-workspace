// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// chatPrivacySQL is used at collection, admin reads, evaluation selection and
// commit. Unknown/staged profiles and orphaned restricted session IDs stay out.
// Hashes are locally rendered SHA-256 hex, never request input.
func chatPrivacySQL() string {
	var hashes []string
	d := instance.Current()
	if d.Enabled("evaluation") {
		for _, p := range d.Assistants {
			if p.Enabled && p.Evaluation {
				hashes = append(hashes, "'"+d.AssistantPin(p).Hash+"'")
			}
		}
	}
	if len(hashes) == 0 {
		hashes = []string{"''"}
	}
	return `((c.session_id NOT LIKE 'ses_profile_%'
 AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.uid=c.uid AND p.folder=c.project AND p.profile_id!='')
 AND NOT EXISTS (SELECT 1 FROM trash t WHERE t.uid=c.uid AND t.folder=c.project AND t.profile_id!='')
 AND NOT EXISTS (SELECT 1 FROM profile_sessions s WHERE s.uid=c.uid AND s.id=c.session_id))
 OR EXISTS (SELECT 1 FROM profile_sessions s JOIN profile_projects pp ON pp.project_id=s.project_id AND pp.uid=s.uid
 JOIN projects p ON p.id=pp.project_id AND p.uid=pp.uid
 WHERE s.id=c.session_id AND s.uid=c.uid AND pp.evaluation=1 AND pp.consent_hash=p.profile_hash AND p.profile_hash IN (` + strings.Join(hashes, ",") + `) AND EXISTS (SELECT 1 FROM consents k WHERE k.uid=c.uid AND k.version='` + strings.ReplaceAll(d.Consent.Version, "'", "''") + `')))`
}

func (s *Store) MayEvaluateChat(ctx context.Context, uid, id, version string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chats c JOIN users u ON u.uid=c.uid JOIN consents k ON k.uid=u.uid
 WHERE c.uid=? AND c.session_id=? AND k.version=? AND `+chatPrivacySQL()+`)`, uid, id, version).Scan(&ok)
	return ok, err
}
