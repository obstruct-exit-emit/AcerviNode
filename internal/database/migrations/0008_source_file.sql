-- source_file/source_file_name let a usenet download added via an uploaded
-- .nzb file support Re-add too — unlike a URL-based add (Source) or a
-- torrent (always reconstructable from just its hash), a file-uploaded NZB
-- has no link to resubmit, only the original bytes. Stored directly on the
-- row rather than as a separate file on disk, deliberately: deleting the row
-- (DELETE FROM downloads) then removes the stored file atomically with it —
-- no separate cleanup step, no possibility of an orphaned file surviving a
-- deleted download. NULL for every other case (a URL-based add, a torrent,
-- a webdl, or a discovered download that was never uploaded to AcerviNode
-- at all).
ALTER TABLE downloads ADD COLUMN source_file BLOB;
ALTER TABLE downloads ADD COLUMN source_file_name TEXT;
