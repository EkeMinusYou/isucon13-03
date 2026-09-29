-- Preserve portal output independently of the entered score and verdict.
create or replace view portal_result_logs_raw as
select regexp_extract(filename, 'runs/([^/]+)/', 1) as run_id,
       content, 'benchmark-result.txt' as source_artifact
from read_text(getvariable('run_glob') || '/benchmark-result.txt');
