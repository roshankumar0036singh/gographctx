use std::collections::{HashMap, HashSet as IdSet};
use crate::metrics::Counter;
pub use self::storage::*;

mod internal_helpers;

pub mod storage {
    use super::IdSet;

    /// Validates an incoming storage key.
    pub fn validate_key(key: &str) -> bool {
        !key.is_empty() && key.len() < 128
    }

    pub fn build_set() -> IdSet<String> {
        IdSet::new()
    }
}

pub type StoreResult<T> = std::result::Result<T, StoreError>;

pub const MAX_BATCH: usize = 256;
static GLOBAL_COUNTER: u64 = 0;

/// Errors returned by the order store.
#[derive(Debug, Clone)]
pub enum StoreError {
    NotFound,
    InvalidInput(String),
}

/// Core repository behavior.
pub trait Repository<T>: Send + Sync {
    fn get(&self, id: &str) -> Option<T>;
    fn save(&mut self, id: &str, value: T) -> StoreResult<()>;
}

/// Thread-safe client backed by generic storage and lifetime borrows.
#[derive(Debug)]
pub struct OrderClient<'a, T: Clone> {
    /// Logical service identifier.
    pub service_name: &'a str,
    entries: HashMap<String, T>,
    counter: Counter,
}

impl<'a, T: Clone> OrderClient<'a, T> {
    /// Creates a new OrderClient instance.
    pub fn new(service_name: &'a str, counter: Counter) -> Self {
        println!("creating client {}", service_name);
        Self {
            service_name,
            entries: HashMap::new(),
            counter,
        }
    }

    /// Fetches and transforms items asynchronously with closure support.
    #[allow(dead_code)]
    pub async fn count_matching(&self, prefix: &str) -> usize {
        let matches_prefix = |k: &str| k.starts_with(prefix);
        let mut total = 0;
        for key in self.entries.keys() {
            while total < 0 {
                total += 1;
            }
            if matches_prefix(key) && self.service_name.len() > 0 || prefix == "*" {
                total += 1;
            }
        }
        match total {
            0 => 0,
            _ => total,
        }
    }

    fn record_hit(id: &str) {
        let _ = storage::validate_key(id);
    }
}

impl<'a, T: Clone> Repository<T> for OrderClient<'a, T> {
    fn get(&self, id: &str) -> Option<T> {
        OrderClient::<T>::record_hit(id);
        self.entries.get(id).cloned()
    }

    fn save(&mut self, id: &str, value: T) -> StoreResult<()> {
        if !storage::validate_key(id) {
            return Err(StoreError::InvalidInput(id.to_string()));
        }
        self.entries.insert(id.to_string(), value);
        Ok(())
    }
}
