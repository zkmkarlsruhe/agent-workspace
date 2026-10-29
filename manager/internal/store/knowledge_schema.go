// SPDX-License-Identifier: MIT

package store

const knowledgeSchema = `
ALTER TABLE users ADD COLUMN incarnation TEXT NOT NULL DEFAULT '';
UPDATE users SET incarnation = lower(hex(randomblob(16)));
CREATE TRIGGER users_incarnation AFTER INSERT ON users BEGIN
 UPDATE users SET incarnation = lower(hex(randomblob(16))) WHERE uid = NEW.uid;
END;
CREATE TABLE teams (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL DEFAULT 1, archived INTEGER NOT NULL DEFAULT 0,
 creator TEXT REFERENCES users(uid) ON DELETE SET NULL, created INTEGER NOT NULL
);
CREATE TABLE team_members (
 team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 uid TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
 role TEXT NOT NULL CHECK(role IN ('admin','member')),
 PRIMARY KEY(team_id, uid)
);
CREATE INDEX team_members_user ON team_members(uid);
CREATE TABLE team_audit (
 id INTEGER PRIMARY KEY, team_id TEXT NOT NULL,
 actor TEXT REFERENCES users(uid) ON DELETE SET NULL,
 subject TEXT REFERENCES users(uid) ON DELETE SET NULL,
 action TEXT NOT NULL, created INTEGER NOT NULL
);
CREATE TABLE knowledge_collections (
 id TEXT PRIMARY KEY,
 owner_uid TEXT REFERENCES users(uid) ON DELETE CASCADE,
 team_id TEXT REFERENCES teams(id),
 title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL DEFAULT 1, archived INTEGER NOT NULL DEFAULT 0,
 created INTEGER NOT NULL,
 CHECK ((owner_uid IS NOT NULL) != (team_id IS NOT NULL))
);
CREATE INDEX knowledge_owner ON knowledge_collections(owner_uid);
CREATE INDEX knowledge_team ON knowledge_collections(team_id);
CREATE TABLE knowledge_pages (
 id TEXT PRIMARY KEY, collection_id TEXT NOT NULL REFERENCES knowledge_collections(id) ON DELETE CASCADE,
 head INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX knowledge_pages_collection ON knowledge_pages(collection_id);
CREATE TABLE knowledge_revisions (
 page_id TEXT NOT NULL REFERENCES knowledge_pages(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, title TEXT NOT NULL, parent TEXT NOT NULL DEFAULT '',
 body TEXT NOT NULL, sources TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 archived INTEGER NOT NULL DEFAULT 0,
 actor TEXT REFERENCES users(uid) ON DELETE SET NULL, created INTEGER NOT NULL,
 PRIMARY KEY(page_id, revision)
);
CREATE TABLE project_knowledge (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 collection_id TEXT NOT NULL REFERENCES knowledge_collections(id) ON DELETE CASCADE,
 PRIMARY KEY(project_id, collection_id)
);
CREATE VIRTUAL TABLE knowledge_search USING fts5(page_id UNINDEXED, title, body, tokenize='unicode61');
CREATE TRIGGER knowledge_page_delete AFTER DELETE ON knowledge_pages BEGIN
 DELETE FROM knowledge_search WHERE page_id = OLD.id;
END;
`
