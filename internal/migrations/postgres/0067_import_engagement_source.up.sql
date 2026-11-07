-- NULL is unknown history; '' marks native writes; nonempty values name import sources.
ALTER TABLE content_favorites ADD COLUMN import_source text;
ALTER TABLE content_reactions ADD COLUMN import_source text;
