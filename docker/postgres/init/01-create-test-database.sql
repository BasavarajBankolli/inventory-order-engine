-- Runs once, when the PostgreSQL data volume is created for the first time.
--
-- Automated integration tests use their own database so they can freely
-- create and wipe data without touching your development data.
CREATE DATABASE inventory_test;
