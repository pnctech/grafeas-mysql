#!/bin/bash
set -e

echo 'Setting up MySQL database'

# Replace with extracting username and password from config
# Set password and utilize username from extracted var
mysql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
    CREATE USER grafeas WITH PASSWORD 'changeme';
    ALTER ROLE grafeas WITH CREATEDB;
    CREATE DATABASE grafeas_db;
    GRANT ALL PRIVILEGES ON DATABASE grafeas_db TO grafeas;
EOSQL

echo 'Database is up'
