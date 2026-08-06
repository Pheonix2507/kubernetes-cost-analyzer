-- Reverse of 000006.
--
-- Dropping a column destroys its data, and for monthly_reports.currency that data is part of an
-- immutable record. There is no way to make that reversible, so the down migration is honest about
-- being lossy rather than pretending otherwise: rolling back and rolling forward again leaves every
-- statement stated in the default currency, whatever it was stated in before.
--
-- The constraints are dropped explicitly rather than left to cascade with the columns. DROP COLUMN
-- does remove them, but naming them here means this file reads as the exact inverse of the up
-- migration, and a reader can check the pair line by line.
ALTER TABLE monthly_reports DROP CONSTRAINT IF EXISTS monthly_reports_currency_iso4217;
ALTER TABLE monthly_reports DROP COLUMN IF EXISTS currency;

ALTER TABLE clusters DROP CONSTRAINT IF EXISTS clusters_currency_iso4217;
ALTER TABLE clusters DROP COLUMN IF EXISTS currency;
ALTER TABLE clusters DROP COLUMN IF EXISTS account;
