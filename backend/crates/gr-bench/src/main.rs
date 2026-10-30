//! B12 · 压测与可靠性基线
//!
//! 两种模式：
//! - `--mode engine`：进程内直接调 `Engine::decide`，测**纯执行**的延迟分位（不含协议栈）；
//! - `--mode http`：起 T 个线程走 keep-alive 打真实服务，测吞吐与端到端延迟。
//!
//! 两种模式必须分开看：`engine` 数字告诉你「引擎本身多快」，`http` 数字告诉你
//! 「这台机器上一条连接能跑多快」。把两者混在一起谈，得到的都是一个不可归因的数字。

mod samples;

use gr_contract::{ExecutionContext, FactCatalog, FactSnapshot, RulePackage};
use gr_engine::{Engine, EngineConfig};
use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let opt = |name: &str| -> Option<String> {
        args.iter().position(|a| a == name).and_then(|i| args.get(i + 1)).cloned()
    };
    let num = |name: &str, default: u64| -> u64 {
        opt(name).and_then(|v| v.parse().ok()).unwrap_or(default)
    };

    let mode = opt("--mode").unwrap_or_else(|| "engine".to_string());
    let rules = num("--rules", 1000) as usize;

    // 生成样本后退出（供服务器加载：`--emit target/bench/rule-package-1000.json`）
    if let Some(path) = opt("--emit") {
        let pkg = samples::bench_package(rules);
        if let Some(dir) = std::path::Path::new(&path).parent() {
            std::fs::create_dir_all(dir).ok();
        }
        std::fs::write(&path, serde_json::to_vec_pretty(&pkg).expect("serialize"))
            .unwrap_or_else(|e| fail(&format!("写 {path} 失败: {e}")));
        println!("已生成 {rules} 条规则包 → {path}");
        return;
    }

    match mode.as_str() {
        "engine" => engine_mode(rules, num("--iters", 20_000) as usize),
        "http" => http_mode(
            &opt("--addr").unwrap_or_else(|| "127.0.0.1:8080".to_string()),
            &opt("--path").unwrap_or_else(|| "/v1/decide".to_string()),
            rules,
            num("--threads", 8) as usize,
            num("--seconds", 5),
        ),
        other => fail(&format!("未知 --mode {other}（engine | http）")),
    }
}

fn fail(msg: &str) -> ! {
    eprintln!("{msg}");
    std::process::exit(2);
}

// ---------------------------------------------------------------- 进程内

fn build_engine(rules: usize) -> (Engine, FactSnapshot, ExecutionContext) {
    let catalog: FactCatalog =
        serde_json::from_value(samples::bench_catalog()).expect("catalog");
    let pkg: RulePackage =
        serde_json::from_value(samples::bench_package(rules)).expect("package");
    let snapshot: FactSnapshot =
        serde_json::from_value(samples::bench_snapshot()).expect("snapshot");

    // 压测要的是「引擎能跑多快」，不是「限额拦不拦得住」——把上限放宽到不是瓶颈
    let config = EngineConfig {
        max_inflight: 1_000_000,
        step_budget: 10_000_000,
        deadline_ms: 10_000,
        ..EngineConfig::default()
    };
    let engine = Engine::new(catalog, vec![pkg], config).expect("engine");

    let mut ctx = ExecutionContext::new("bench", samples::BENCH_SCENE, "2026-10-30T10:00:00Z");
    ctx.ruleset_id = Some(samples::ruleset_id(rules));
    (engine, snapshot, ctx)
}

fn engine_mode(rules: usize, iters: usize) {
    let (engine, snapshot, ctx) = build_engine(rules);
    let stats = engine.stats().pop().expect("stats");

    // 预热：编译器/分支预测/页表都要先热起来，否则前几百次全是冷启动噪音
    for _ in 0..1000 {
        engine.decide(&snapshot, &ctx).expect("warmup");
    }

    let mut samples_ns = Vec::with_capacity(iters);
    let started = Instant::now();
    for _ in 0..iters {
        let t = Instant::now();
        let out = engine.decide(&snapshot, &ctx).expect("decide");
        samples_ns.push(t.elapsed().as_nanos() as u64);
        // 防优化：让结果真的被用一下
        std::hint::black_box(out.result.decision);
    }
    let total = started.elapsed();

    samples_ns.sort_unstable();
    let qps = iters as f64 / total.as_secs_f64();
    println!(
        "mode=engine rules={} instrs={} iters={} qps={:.0} p50={} p95={} p99={} max={} mean={}",
        rules,
        stats.instrs,
        iters,
        qps,
        us(pct(&samples_ns, 50.0)),
        us(pct(&samples_ns, 95.0)),
        us(pct(&samples_ns, 99.0)),
        us(*samples_ns.last().unwrap()),
        us(samples_ns.iter().sum::<u64>() / samples_ns.len() as u64),
    );
}

// ---------------------------------------------------------------- HTTP

fn http_mode(addr: &str, path: &str, rules: usize, threads: usize, seconds: u64) {
    // 容错：允许 `v1/trial` 与 `/v1/trial` 两种写法（Git Bash 会把以 / 开头的参数转成 Windows 路径）
    let path = format!("/{}", path.trim_start_matches('/'));
    let snapshot = samples::bench_snapshot();
    let body = serde_json::json!({
        "context": {
            "tenantId": "bench",
            "scene": samples::BENCH_SCENE,
            "requestTime": "2026-10-30T10:00:00Z",
            "rulesetId": samples::ruleset_id(rules)
        },
        "snapshot": snapshot
    })
    .to_string();
    let request = format!(
        "POST {path} HTTP/1.1\r\nHost: {addr}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: keep-alive\r\n\r\n{}",
        body.len(),
        body
    );

    let ok = Arc::new(AtomicU64::new(0));
    let failed = Arc::new(AtomicU64::new(0));
    let deadline = Instant::now() + Duration::from_secs(seconds);

    let mut handles = Vec::with_capacity(threads);
    for _ in 0..threads {
        let request = request.clone();
        let addr = addr.to_string();
        let path = path.clone();
        let ok = Arc::clone(&ok);
        let failed = Arc::clone(&failed);
        handles.push(std::thread::spawn(move || {
            let mut stream = match TcpStream::connect(&addr) {
                Ok(s) => s,
                Err(_) => {
                    failed.fetch_add(1, Ordering::Relaxed);
                    return Vec::new();
                }
            };
            stream.set_nodelay(true).ok();
            stream.set_read_timeout(Some(Duration::from_secs(5))).ok();
            let mut lat = Vec::new();
            let mut buf = vec![0u8; 8192];
            while Instant::now() < deadline {
                let t = Instant::now();
                if stream.write_all(request.as_bytes()).is_err() {
                    failed.fetch_add(1, Ordering::Relaxed);
                    break;
                }
                // 读一个完整响应（头部 + Content-Length 指定长度）
                match read_response(&mut stream, &mut buf) {
                    Some(200) => {
                        lat.push(t.elapsed().as_nanos() as u64);
                        ok.fetch_add(1, Ordering::Relaxed);
                    }
                    Some(status) => {
                        // 非 200 绝不算成功：压测把 4xx/5xx 记成吞吐是最常见的自欺
                        eprintln!("收到 HTTP {status}（{path}）");
                        failed.fetch_add(1, Ordering::Relaxed);
                        break;
                    }
                    None => {
                        failed.fetch_add(1, Ordering::Relaxed);
                        break;
                    }
                }
            }
            lat
        }));
    }

    let started = Instant::now();
    let mut all = Vec::new();
    for h in handles {
        if let Ok(mut v) = h.join() {
            all.append(&mut v);
        }
    }
    let elapsed = started.elapsed().as_secs_f64();
    all.sort_unstable();

    let ok = ok.load(Ordering::Relaxed);
    println!(
        "mode=http addr={addr} path={path} rules={} threads={} seconds={:.1} ok={} failed={} qps={:.0} p50={} p95={} p99={} max={}",
        rules, threads, elapsed, ok, failed.load(Ordering::Relaxed),
        ok as f64 / elapsed,
        us(pct(&all, 50.0)), us(pct(&all, 95.0)), us(pct(&all, 99.0)), us(*all.last().unwrap_or(&0)),
    );
}

fn read_response(stream: &mut TcpStream, buf: &mut [u8]) -> Option<u16> {
    let mut head = Vec::with_capacity(512);
    loop {
        let n = stream.read(buf).ok()?;
        if n == 0 {
            return None;
        }
        head.extend_from_slice(&buf[..n]);
        if let Some(pos) = find(&head, b"\r\n\r\n") {
            let header = String::from_utf8_lossy(&head[..pos]).to_string();
            let status = header
                .lines()
                .next()
                .and_then(|l| l.split_whitespace().nth(1))
                .and_then(|s| s.parse::<u16>().ok())?;
            let len = header
                .lines()
                .filter_map(|l| l.split_once(':'))
                .find(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                .and_then(|(_, v)| v.trim().parse::<usize>().ok())?;
            let body_have = head.len() - (pos + 4);
            if body_have >= len {
                return Some(status);
            }
            // 继续读 body
            let mut remaining = len - body_have;
            let mut chunk = vec![0u8; 4096];
            while remaining > 0 {
                let n = stream.read(&mut chunk).ok()?;
                if n == 0 {
                    return None;
                }
                remaining = remaining.saturating_sub(n);
            }
            return Some(status);
        }
    }
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack.windows(needle.len()).position(|w| w == needle)
}

fn pct(sorted: &[u64], p: f64) -> u64 {
    if sorted.is_empty() {
        return 0;
    }
    let idx = ((sorted.len() as f64 - 1.0) * p / 100.0).round() as usize;
    sorted[idx.min(sorted.len() - 1)]
}

fn us(ns: u64) -> String {
    format!("{:.1}us", ns as f64 / 1000.0)
}
