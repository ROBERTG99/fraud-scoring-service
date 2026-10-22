-- Runs once, when the compose Postgres volume is created (docker-entrypoint-initdb.d). The fraud schema and
-- its roles come from goose migrations (`migrate` job); MLflow keeps its registry in a database of its own.
-- Development password only, as in migrations/00001_roles.sql.
CREATE ROLE mlflow LOGIN PASSWORD 'mlflow';
CREATE DATABASE mlflow OWNER mlflow;
