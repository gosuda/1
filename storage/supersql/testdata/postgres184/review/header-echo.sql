\set VERBOSITY terse
SELECT 1 AS "SELECT missing_echo_probe();";
\echo HEADER_LITERAL :SQLSTATE
SELECT missing_echo_probe();
\echo HEADER_MISSING :SQLSTATE
