-- ISUCON13 logs use ISO timestamps and a final tip total. The acquired
-- official specification publishes no separate addition/deduction formula.
create table if not exists bench_error_logs (run_id varchar, content varchar, source_artifact varchar);
create table if not exists portal_result_logs (run_id varchar, content varchar, source_artifact varchar);
create or replace view isupipe_result_logs as
select * from bench_error_logs
union all select * from portal_result_logs;

create or replace view bench_results as
with parsed as (
    select run_id, source_artifact,
        try_cast(list_last(regexp_extract_all(content, '(?:スコア|SCORE|score):\s*([0-9]+)', 1)) as bigint) as score,
        regexp_matches(content, '(?m)^BENCHMARK_FAIL(?:\s|$)|(?:初期化|整合性チェック|最終チェック|ベンチマーク走行)が失敗しました') as failed,
        regexp_matches(content, '(?m)^BENCHMARK_PASS(?:\s|$)|最終チェックが成功しました') as succeeded
    from isupipe_result_logs
)
select run_id,
    coalesce(max(score) filter (where source_artifact = 'bench.log'), max(score)) as score,
    case when bool_or(failed) then false when bool_or(succeeded) then true else null end as passed,
    cast(null as bigint) as addition, cast(null as bigint) as deduction,
    string_agg(distinct source_artifact, ', ' order by source_artifact) as source_artifact
from parsed group by run_id;

-- No route points or explicit penalties occur in the acquired output.
create or replace view bench_score_routes as
select cast(null as varchar) as run_id, cast(null as varchar) as method,
       cast(null as varchar) as route, cast(null as bigint) as points,
       cast(null as varchar) as source_artifact where false;

create or replace view isupipe_bench_lines as
with split_logs as (
    select run_id, source_artifact, string_split(content, chr(10)) as lines
    from isupipe_result_logs
), expanded_lines as (
    -- Expand in SELECT to avoid a lateral join keyed by the entire log.
    select run_id, source_artifact, unnest(lines) as line,
           generate_subscripts(lines, 1) as line_number
    from split_logs
)
select run_id, source_artifact, cast(line_number as bigint) as line_number, line,
       try_cast(regexp_extract(line, '^([0-9]{4}-[0-9]{2}-[0-9]{2}T[^\t ]+)', 1) as timestamptz) as occurred_at
from expanded_lines;

create or replace view bench_errors as
select run_id, line_number, cast(occurred_at as time) as reported_time,
       cast(null as bigint) as error_index, line as message,
       case when occurred_at is null then 'completion_summary' else 'event_time' end as time_semantics,
       source_artifact
from isupipe_bench_lines
where regexp_matches(line, '\t(?:warn|error)\t') or starts_with(line, 'BENCHMARK_FAIL');

create or replace view bench_warning_events as
-- Several independent requests can fail at the same millisecond.
select run_id, occurred_at from isupipe_bench_lines
where occurred_at is not null and regexp_matches(line, '\t(?:warn|error)\t');

-- No cumulative error-count time series is present in the observed output.
create or replace view bench_error_counts as
select cast(null as varchar) as run_id, cast(null as bigint) as line_number,
       cast(null as double) as elapsed_s, cast(null as bigint) as error_count where false;

-- Scenario counts are the official output's operation breakdown, not points.
create or replace view isupipe_scenario_counts as
select run_id,
    regexp_extract(line, '\[シナリオ ([^\]]+)\]', 1) as scenario,
    try_cast(regexp_extract(line, '\] ([0-9]+) 回成功', 1) as bigint) as successes,
    coalesce(try_cast(regexp_extract(line, ', ([0-9]+) 回失敗', 1) as bigint), 0) as failures,
    source_artifact
from isupipe_bench_lines where contains(line, '[シナリオ ');
