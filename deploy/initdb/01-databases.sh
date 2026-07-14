#!/bin/bash
# Runs once, on an empty data directory, by the postgres image's own convention.
#
# A shell hook rather than a .sql one because the app role's password comes from
# the environment: the image runs plain SQL with no variable expansion, so the
# alternative was substituting a placeholder before mounting it, which puts the
# password in a file on disk to avoid putting it in a file on disk.
#
# Two databases and one NON-SUPERUSER role. The role is the load-bearing part:
# the control plane refuses to start as anything holding BYPASSRLS, because a
# role that can see through the row-level security policies makes every one of
# them decorative -- and this deployment is multi-tenant by construction.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-SQL
	CREATE DATABASE lemul;
	CREATE DATABASE logto;
	-- LiteLLM's own store. Not optional if the admin UI or spend logs are
	-- wanted: without a database it answers "Not connected to DB!" at login
	-- and /spend/logs returns nothing, which is the signal decision #12 rests
	-- on.
	CREATE DATABASE litellm;
SQL

# Quoted through psql's own parameter binding rather than interpolated into the
# statement, so a password containing a quote cannot end the string early.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" \
	-v pw="$APP_DB_PASSWORD" <<-SQL
	CREATE ROLE lemul_app LOGIN PASSWORD :'pw' NOBYPASSRLS;
	GRANT CONNECT ON DATABASE lemul TO lemul_app;
	-- LiteLLM manages its own schema, so unlike lemul_app this role owns its
	-- database and needs DDL.
	CREATE ROLE litellm LOGIN PASSWORD :'pw';
	ALTER DATABASE litellm OWNER TO litellm;
SQL

# Table grants are NOT here. The schema does not exist yet -- the control plane
# applies it at boot with the superuser DSN and grants the app role afterwards,
# which is also why a GRANT ... ON ALL TABLES run now would cover nothing.
