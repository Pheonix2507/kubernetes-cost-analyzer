-- Cluster metadata, so a fleet of clusters is describable rather than merely countable.
--
-- WHY THIS MIGRATION EXISTS
-- ------------------------
-- `clusters` has carried `provider` and `region` since the baseline, and nothing has ever populated
-- them: cmd/collector/writer.go passed the literals "kubernetes" and "". Columns that exist and are
-- always the same value are worse than absent ones, because a reader reasonably assumes they mean
-- something. Phase 11 makes them real and adds the two attributes a fleet actually needs.

-- WHY `account` AND NOT SOMETHING MORE GENERAL
-- Two clusters called `prod`, one in each of two AWS accounts, are different clusters that will
-- appear on different invoices. `account` is the column that distinguishes them, and it is the ONE
-- piece of cluster identity that cannot be discovered from inside the cluster: a pod can read its
-- own nodes' providerID and region labels, but nothing in the Kubernetes API names the billing
-- account that owns them. So this is configured, while provider and region are derived.
--
-- DEFAULT '' rather than NULL. An empty account means "not recorded", the same as today's clusters,
-- and it keeps every query free of IS NULL branches. NULL would make `account = ''` and
-- `account IS NULL` two different kinds of unknown, which is one kind too many.
ALTER TABLE clusters ADD COLUMN account text NOT NULL DEFAULT '';

-- WHY CURRENCY IS ON THE CLUSTER, AND WHY IT IS NOT ON THE FACT TABLE
-- ------------------------------------------------------------------
-- Cost figures without a currency are not figures. Once a fleet spans regions or providers, the
-- rate catalogues behind them may be denominated differently, and summing them produces a number
-- that is confidently wrong in a way no test detects: 100 USD + 100 EUR = 200 of nothing.
--
-- It lives here because currency is a property of the RATES used to compute a cost, and rates are
-- selected by (provider, region) -- which is to say, per cluster. Denormalising it onto
-- container_allocations would add a text column to the largest table in the schema to record a value
-- that is identical for every row sharing a cluster.
--
-- The honest cost of that choice: changing a cluster's currency retroactively reinterprets all of
-- its history, because old rows have no currency of their own to contradict the new one. That is
-- therefore a deliberate data migration, not an ordinary UPDATE, and this comment is the warning.
ALTER TABLE clusters ADD COLUMN currency text NOT NULL DEFAULT 'USD';

-- ISO 4217 codes are exactly three uppercase letters. The constraint is worth having because the
-- value arrives from a YAML catalogue a human edits, and `usd` or `US$` would otherwise flow all the
-- way to a rendered dashboard before anyone noticed.
ALTER TABLE clusters
  ADD CONSTRAINT clusters_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$');

-- WHY UNIQUE (name) IS DELIBERATELY LEFT ALONE
-- --------------------------------------------
-- The obvious move once `account` exists is to make the natural key
-- (provider, account, region, name), permitting two clusters to share a name. It is rejected on
-- purpose.
--
-- Every fact row, every rollup row and every monthly statement identifies its cluster by NAME, not
-- by id. A composite natural key would mean every one of those carries four columns instead of one,
-- every API filter takes four parameters, and every URL that names a cluster becomes unreadable.
-- The benefit only materialises for an operator who insists on keeping two clusters with the same
-- name, which is a naming problem they can solve for free.
--
-- So the rule is: CLUSTER NAMES MUST BE UNIQUE ACROSS THE FLEET, and provider, account and region
-- are attributes describing a cluster rather than parts of its identity. This is the same choice
-- Kubecost and OpenCost make with their cluster_id. The Phase 11a.2 ingest boundary reinforces it,
-- since a per-cluster token maps to exactly one cluster name.

-- A monthly statement is an immutable record of what we said, frozen by trigger. A frozen figure
-- with no currency attached is not self-contained: read it back in two years, after the catalogue
-- has been re-denominated, and nothing in the row says which currency it was stated in. The whole
-- point of finalising is that the row can be trusted alone.
--
-- Adding a column with a default is metadata-only in PostgreSQL 11 and later, and DDL does not fire
-- row-level UPDATE triggers, so this does not disturb the 10 already-finalised statements.
ALTER TABLE monthly_reports ADD COLUMN currency text NOT NULL DEFAULT 'USD';

ALTER TABLE monthly_reports
  ADD CONSTRAINT monthly_reports_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$');

COMMENT ON COLUMN clusters.account IS
  'Billing account, project or subscription that owns the cluster. Configured via CLUSTER_ACCOUNT: it cannot be discovered from inside the cluster. Empty means not recorded.';
COMMENT ON COLUMN clusters.provider IS
  'Derived from the scheme of a node spec.providerID: aws, gce, azure, kind. Refreshed on every collector cycle.';
COMMENT ON COLUMN clusters.region IS
  'Derived from the topology.kubernetes.io/region label on the cluster nodes. Empty when the nodes disagree or carry no region label.';
COMMENT ON COLUMN clusters.currency IS
  'ISO 4217 code of the rate catalogue used to price this cluster. Changing it reinterprets all existing history, so treat it as a data migration.';
COMMENT ON COLUMN monthly_reports.currency IS
  'Currency the statement was stated in, denormalised so a finalised row is self-contained.';
