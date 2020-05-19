CREATE TABLE packs (
    id         INTEGER PRIMARY KEY,
    sum        BLOB NOT NULL,
    num_chunks INTEGER NOT NULL,
    size       INTEGER NOT NULL,
    file_id    TEXT NOT NULL,

    CHECK (length(sum) = 32),
    CHECK (num_chunks > 0),
    CHECK (size > 0),
    CHECK (length(file_id) > 0)
);

CREATE TABLE indexes (
    id            INTEGER PRIMARY KEY,
    pack          INTEGER NOT NULL REFERENCES packs (id),
    sequence      INTEGER NOT NULL,
    sum           BLOB NOT NULL,
    chunk_size    INTEGER NOT NULL,
    mode          INTEGER NOT NULL,
    offset        INTEGER NOT NULL,
    size          INTEGER NOT NULL

    CHECK (sequence >= 0),
    CHECK (length(sum) = 32),
    CHECK (chunk_size > 0),
    CHECK (mode >= 0),
    CHECK (offset >= 0),
    CHECK (size > 0)
);
CREATE INDEX indexes_sum_index ON indexes (sum);


CREATE TABLE files (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL,

    CHECK (length(name) > 0)
);
CREATE INDEX files_name_index ON files(name);


CREATE TABLE file_versions (
    id         INTEGER PRIMARY KEY,
    file       INTEGER NOT NULL REFERENCES files (id),
    created_at INTEGER NOT NULL,
    size       INTEGER NOT NULL,
    num_chunks INTEGER NOT NULL,
    sum        BLOB NOT NULL,

    CHECK (created_at > 0),
    CHECK (size >= 0),
    CHECK (num_chunks >= 0),
    CHECK (length(sum) = 32)
);
CREATE UNIQUE INDEX file_versions_sum_index ON file_versions(sum);


CREATE TABLE file_contents (
    file_version  INTEGER NOT NULL REFERENCES file_versions (id),
    idx           INTEGER NOT NULL REFERENCES indexes (id),
    sequence      INTEGER NOT NULL,

    CHECK (sequence >= 0)
);