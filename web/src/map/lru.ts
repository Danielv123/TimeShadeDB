// A least-recently-used map bounded by a byte budget. Entries can be pinned (for example the
// textures drawn in the current frame) so eviction never removes something still in use.

export interface LruOptions<K, V> {
  /** Maximum total bytes before eviction starts. */
  budget: number;
  /** Called after an entry leaves the cache (evicted, replaced or deleted). */
  onEvict?(key: K, value: V): void;
}

interface Entry<V> {
  value: V;
  bytes: number;
  pin: number;
}

export class ByteLru<K, V> {
  // Map iteration order is insertion order: the first entry is the least recently used.
  private readonly map = new Map<K, Entry<V>>();
  private total = 0;
  budget: number;
  private readonly onEvict?: (key: K, value: V) => void;

  constructor(opts: LruOptions<K, V>) {
    this.budget = opts.budget;
    this.onEvict = opts.onEvict;
  }

  get bytes(): number {
    return this.total;
  }

  get size(): number {
    return this.map.size;
  }

  has(key: K): boolean {
    return this.map.has(key);
  }

  /** Returns the value and marks it most recently used. */
  get(key: K): V | undefined {
    const e = this.map.get(key);
    if (!e) {
      return undefined;
    }
    this.map.delete(key);
    this.map.set(key, e);
    return e.value;
  }

  /** Returns the value without touching recency. */
  peek(key: K): V | undefined {
    return this.map.get(key)?.value;
  }

  set(key: K, value: V, bytes: number): void {
    const old = this.map.get(key);
    if (old) {
      this.map.delete(key);
      this.total -= old.bytes;
      if (old.value !== value) {
        this.onEvict?.(key, old.value);
      }
    }
    this.map.set(key, { value, bytes, pin: old?.pin ?? 0 });
    this.total += bytes;
    this.evict();
  }

  /** Changes the byte size of an entry (e.g. after an in-place update). */
  resize(key: K, bytes: number): void {
    const e = this.map.get(key);
    if (e) {
      this.total += bytes - e.bytes;
      e.bytes = bytes;
      this.evict();
    }
  }

  /** Moves an entry to a new key, keeping its value, recency and size. */
  rekey(from: K, to: K): boolean {
    const e = this.map.get(from);
    if (!e || this.map.has(to)) {
      return false;
    }
    this.map.delete(from);
    this.map.set(to, e);
    return true;
  }

  delete(key: K): boolean {
    const e = this.map.get(key);
    if (!e) {
      return false;
    }
    this.map.delete(key);
    this.total -= e.bytes;
    this.onEvict?.(key, e.value);
    return true;
  }

  pin(key: K): void {
    const e = this.map.get(key);
    if (e) {
      e.pin++;
    }
  }

  unpin(key: K): void {
    const e = this.map.get(key);
    if (e && e.pin > 0) {
      e.pin--;
    }
  }

  /** Clears every pin (call at the start of a frame before pinning what it draws). */
  unpinAll(): void {
    for (const e of this.map.values()) {
      e.pin = 0;
    }
  }

  clear(): void {
    const entries = [...this.map];
    this.map.clear();
    this.total = 0;
    for (const [k, e] of entries) {
      this.onEvict?.(k, e.value);
    }
  }

  keys(): IterableIterator<K> {
    return this.map.keys();
  }

  /** Evicts unpinned entries, oldest first, until the total fits the budget. */
  evict(): void {
    if (this.total <= this.budget) {
      return;
    }
    for (const [k, e] of this.map) {
      if (this.total <= this.budget) {
        break;
      }
      if (e.pin > 0) {
        continue;
      }
      this.map.delete(k);
      this.total -= e.bytes;
      this.onEvict?.(k, e.value);
    }
  }
}
