//! Separates the password from a PostgreSQL connection string, so a saved
//! source can keep the password in the OS credential store and everything else
//! in profiles.json. Both forms pg-dac accepts are handled: the URI form
//! (`postgres://user:secret@host/db?password=…`) and libpq's keyword/value form
//! (`host=… password='quoted value'`).

/// What a connection string holds, password-wise.
#[derive(Debug, PartialEq, Eq)]
pub enum Split {
    /// No password: the string can be stored as it is.
    NoPassword,
    /// The string with every password removed.
    Stripped(String),
}

/// Splits the password out of `dsn`. An error means the string mentions a
/// password in a way this parser does not understand; the caller must then
/// refuse to store it rather than risk writing the password to disk.
pub fn split(dsn: &str) -> Result<Split, String> {
    let s = dsn.trim();
    let stripped = match strip_scheme(s) {
        Some((scheme, rest)) => strip_uri(scheme, rest),
        None => strip_keyword_value(s)?,
    };
    match stripped {
        None if mentions_password(s) => Err(unparsed()),
        None => Ok(Split::NoPassword),
        Some(out) if mentions_password(&out) => Err(unparsed()),
        Some(out) => Ok(Split::Stripped(out)),
    }
}

fn unparsed() -> String {
    "the connection string mentions a password in a form pg-dac cannot separate from the rest, \
     so it was not saved; save it without the password and set PGPASSWORD or a .pgpass file instead"
        .to_string()
}

/// A last check on what will be written to disk: no `password` keyword or
/// parameter, in any case, may remain.
fn mentions_password(s: &str) -> bool {
    let lower = s.to_ascii_lowercase();
    lower
        .match_indices("password")
        .any(|(i, _)| lower[i + "password".len()..].trim_start().starts_with('='))
}

fn strip_scheme(s: &str) -> Option<(&str, &str)> {
    ["postgresql://", "postgres://"]
        .into_iter()
        .find_map(|scheme| {
            let n = scheme.len();
            (s.len() >= n && s.is_char_boundary(n) && s[..n].eq_ignore_ascii_case(scheme))
                .then(|| (&s[..n], &s[n..]))
        })
}

/// The URI form: the password is the part of the userinfo after the first ':'
/// (the userinfo ends at the last '@' of the authority), and a `password`
/// query parameter.
fn strip_uri(scheme: &str, rest: &str) -> Option<String> {
    let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(end);
    let mut changed = false;

    let authority = match authority.rfind('@') {
        Some(at) => {
            let (userinfo, hosts) = authority.split_at(at);
            match userinfo.find(':') {
                Some(colon) => {
                    changed = true;
                    format!("{}{}", &userinfo[..colon], hosts)
                }
                None => authority.to_string(),
            }
        }
        None => authority.to_string(),
    };

    let tail = match tail.find('?') {
        Some(q) => {
            let (path, query) = (&tail[..q], &tail[q + 1..]);
            let (query, fragment) = match query.find('#') {
                Some(h) => query.split_at(h),
                None => (query, ""),
            };
            let kept: Vec<&str> = query
                .split('&')
                .filter(|kv| {
                    let drop = kv
                        .split('=')
                        .next()
                        .unwrap_or("")
                        .eq_ignore_ascii_case("password");
                    changed |= drop;
                    !drop && !kv.is_empty()
                })
                .collect();
            if kept.is_empty() {
                format!("{path}{fragment}")
            } else {
                format!("{path}?{}{fragment}", kept.join("&"))
            }
        }
        None => tail.to_string(),
    };

    changed.then(|| format!("{scheme}{authority}{tail}"))
}

/// libpq's keyword/value form: `key = value` pairs separated by whitespace,
/// a value either bare or in single quotes with `\'` and `\\` escapes.
/// `Ok(None)` means no password keyword was found (or the text is not in this
/// form, which the final check in `split` then judges).
fn strip_keyword_value(s: &str) -> Result<Option<String>, String> {
    let chars: Vec<char> = s.chars().collect();
    let skip_space = |mut i: usize| {
        while i < chars.len() && chars[i].is_whitespace() {
            i += 1;
        }
        i
    };
    let mut kept: Vec<String> = Vec::new();
    let mut changed = false;
    let mut i = skip_space(0);
    while i < chars.len() {
        let key_start = i;
        while i < chars.len() && !chars[i].is_whitespace() && chars[i] != '=' {
            i += 1;
        }
        let key: String = chars[key_start..i].iter().collect();
        i = skip_space(i);
        if i >= chars.len() || chars[i] != '=' {
            return Ok(None);
        }
        i = skip_space(i + 1);
        let value_start = i;
        if i < chars.len() && chars[i] == '\'' {
            i += 1;
            loop {
                match chars.get(i) {
                    None => {
                        return Err("unterminated quoted value in the connection string".to_string())
                    }
                    Some('\\') => i += 2,
                    Some('\'') => {
                        i += 1;
                        break;
                    }
                    Some(_) => i += 1,
                }
            }
        } else {
            while i < chars.len() && !chars[i].is_whitespace() {
                i += 1;
            }
        }
        let value: String = chars[value_start..i.min(chars.len())].iter().collect();
        if key == "password" {
            changed = true;
        } else {
            kept.push(format!("{key}={value}"));
        }
        i = skip_space(i);
    }
    Ok(changed.then(|| kept.join(" ")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn stripped(dsn: &str) -> String {
        match split(dsn) {
            Ok(Split::Stripped(s)) => s,
            other => panic!("{dsn}: expected a stripped string, got {other:?}"),
        }
    }

    #[test]
    fn uri_userinfo() {
        assert_eq!(
            stripped("postgres://app:s3cret@db.example:5432/app?sslmode=require"),
            "postgres://app@db.example:5432/app?sslmode=require"
        );
        assert_eq!(
            stripped("postgresql://app:p%40ss:w0rd@h1,h2/app"),
            "postgresql://app@h1,h2/app"
        );
        // An unencoded '@' in the password: the userinfo ends at the last '@'.
        assert_eq!(
            stripped("postgres://app:a@b@host/db"),
            "postgres://app@host/db"
        );
        assert_eq!(stripped("POSTGRES://app:x@host"), "POSTGRES://app@host");
        assert_eq!(stripped("postgres://:x@host/db"), "postgres://@host/db");
    }

    #[test]
    fn uri_query_parameter() {
        assert_eq!(
            stripped("postgres://app@host/db?password=x&sslmode=require"),
            "postgres://app@host/db?sslmode=require"
        );
        assert_eq!(
            stripped("postgres://app@host/db?sslmode=require&PASSWORD=x"),
            "postgres://app@host/db?sslmode=require"
        );
        assert_eq!(
            stripped("postgres://app:y@host/db?password=x"),
            "postgres://app@host/db"
        );
    }

    #[test]
    fn keyword_value() {
        assert_eq!(
            stripped("host=db port=5432 user=app password=s3cret dbname=app"),
            "host=db port=5432 user=app dbname=app"
        );
        assert_eq!(
            stripped(r"host=db password = 'it\'s a secret' dbname=app"),
            "host=db dbname=app"
        );
        assert_eq!(stripped("password='' host=db"), "host=db");
        assert_eq!(
            stripped("host=db application_name='a b' password=x"),
            "host=db application_name='a b'"
        );
    }

    #[test]
    fn no_password() {
        for dsn in [
            "postgres://app@host/db?sslmode=require",
            "postgres://host/db",
            "host=db user=app dbname=app",
            "host=db passfile=/home/app/.pgpass",
        ] {
            assert_eq!(split(dsn), Ok(Split::NoPassword), "{dsn}");
        }
    }

    #[test]
    fn refuses_what_it_cannot_parse() {
        // The .NET form is not one pg-dac accepts, and its password cannot be
        // located safely: it is refused rather than written to disk.
        assert!(split("Host=db;Username=app;Password=s3cret;Database=app").is_err());
        assert!(split("host=db password='unterminated").is_err());
    }

    #[test]
    fn never_returns_the_password() {
        for dsn in [
            "postgres://app:s3cret@host/db",
            "postgres://app@host/db?password=s3cret",
            "host=db password=s3cret",
            "host=db password='s3cret'",
            "postgres://app:s3cret@host/db?password=s3cret",
        ] {
            assert!(!stripped(dsn).contains("s3cret"), "{dsn}");
        }
    }
}
