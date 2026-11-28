\set VERBOSITY terse
SELECT 'SELECT missing_echo_probe();' AS echoed;
\echo ECHO_LITERAL :SQLSTATE
SELECT missing_echo_probe();
\echo ECHO_MISSING :SQLSTATE
