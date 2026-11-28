\set VERBOSITY terse
    SELECT missing_indented_echo_probe();
\echo INDENTED_STATE :SQLSTATE
------------------------------
    SELECT missing_divider_echo_probe();
\echo (1 row)
\echo DIVIDER_STATE :SQLSTATE
