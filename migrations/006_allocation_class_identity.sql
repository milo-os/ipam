-- 006: record exactly which class, and what address scope, an allocation was
-- handed out under.
--
-- WHY
--
-- A claim recreated under the name of one that retained its address takes the
-- address back, but only on the terms it was held under. class_name alone does
-- not say which class that was: it is the definition's name, and two projects
-- can each define a class of one name. A consumer's own `standard` and a
-- reference to the platform's `standard` resolve to different classes, and a
-- chain whose parent crosses into another project can put both on the same
-- leaf pool, so neither the name nor the pool key tells them apart. The
-- defining project does.
--
-- address_scope is the class's spec.addressScope when the address was handed
-- out. It can be set on a class that had none (it is immutable only once set),
-- so the class read at reclaim time is not evidence of what the address was
-- allocated as.
--
-- NULL MEANS "NOT RECORDED"
--
-- Both columns are NULL on every row written before this migration, and on
-- rows that are not a claim's. There is no backfill, because neither value is
-- recoverable: the class may have changed or gone, and the claim that resolved
-- it is deleted. The reclaim path treats NULL as unknown and refuses whatever
-- it cannot prove (internal/allocator/reclaim.go), which a release of the
-- retained allocation then gets past.
--
-- A new claim row writes both, together, and an empty address_scope is a
-- recorded "unset", distinct from NULL.

-- +goose Up

ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS class_project TEXT;
ALTER TABLE ipam_cidr_allocations ADD COLUMN IF NOT EXISTS address_scope TEXT;

COMMENT ON COLUMN ipam_cidr_allocations.class_project IS
    'Project holding the definition of the class this allocation was handed '
    'out under; with class_name, the class identity. NULL when not recorded.';
COMMENT ON COLUMN ipam_cidr_allocations.address_scope IS
    'spec.addressScope of that class at allocation time; empty means unset. '
    'NULL when not recorded.';

-- +goose Down

ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS address_scope;
ALTER TABLE ipam_cidr_allocations DROP COLUMN IF EXISTS class_project;
