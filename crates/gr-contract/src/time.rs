//! 极简 RFC3339 解析（UTC / 带偏移），只到「秒」。
//!
//! 不引 chrono 的理由：引擎只用它算事实新鲜度（`freshnessSec = requestTime - eventTime`），
//! 不需要时区库的全部能力；少一个依赖，少一类非确定性来源。

/// 解析 `2026-10-30T10:00:00Z` / `2026-10-30T10:00:00.123Z` / `+08:00` 偏移，返回 epoch 秒。
pub fn parse_rfc3339_secs(s: &str) -> Option<i64> {
    let s = s.trim();
    let bytes = s.as_bytes();
    if bytes.len() < 19 {
        return None;
    }
    if bytes[4] != b'-' || bytes[7] != b'-' || (bytes[10] != b'T' && bytes[10] != b' ') {
        return None;
    }
    let year: i64 = s.get(0..4)?.parse().ok()?;
    let month: i64 = s.get(5..7)?.parse().ok()?;
    let day: i64 = s.get(8..10)?.parse().ok()?;
    let hour: i64 = s.get(11..13)?.parse().ok()?;
    let min: i64 = s.get(14..16)?.parse().ok()?;
    let sec: i64 = s.get(17..19)?.parse().ok()?;
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }
    if hour > 23 || min > 59 || sec > 60 {
        return None;
    }

    let mut rest = &s[19..];
    // 跳过小数秒
    if let Some(stripped) = rest.strip_prefix('.') {
        let end = stripped.find(|c: char| !c.is_ascii_digit()).unwrap_or(stripped.len());
        rest = &stripped[end..];
    }
    let offset_secs: i64 = match rest {
        "" | "Z" | "z" => 0,
        other => {
            let sign = match other.as_bytes().first()? {
                b'+' => 1,
                b'-' => -1,
                _ => return None,
            };
            let body = &other[1..];
            let (h, m) = match body.len() {
                5 if body.as_bytes()[2] == b':' => (&body[0..2], &body[3..5]),
                4 => (&body[0..2], &body[2..4]),
                2 => (&body[0..2], "0"),
                _ => return None,
            };
            let oh: i64 = h.parse().ok()?;
            let om: i64 = m.parse().ok()?;
            sign * (oh * 3600 + om * 60)
        }
    };

    // days_from_civil（Howard Hinnant），对 1970 起算
    let y = if month <= 2 { year - 1 } else { year };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (month + 9) % 12;
    let doy = (153 * mp + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;

    Some(days * 86_400 + hour * 3600 + min * 60 + sec - offset_secs)
}

#[cfg(test)]
mod tests {
    use super::parse_rfc3339_secs;

    #[test]
    fn parses_utc_and_offsets() {
        assert_eq!(parse_rfc3339_secs("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(parse_rfc3339_secs("2026-10-30T10:00:00Z"), Some(1_793_354_400));
        // 带毫秒与 +08:00 偏移
        let a = parse_rfc3339_secs("2026-10-30T18:00:00.500+08:00").unwrap();
        assert_eq!(a, 1_793_354_400);
        assert!(parse_rfc3339_secs("2026-13-01T00:00:00Z").is_none());
        assert!(parse_rfc3339_secs("nonsense").is_none());
    }
}
