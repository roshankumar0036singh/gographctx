/** Module utilities for TypeScript order service. */

import type { Config, Logger } from "./config";
import * as path from "path";
import DefaultClient, { normalizeId as normalize, type RetryPolicy } from "./client";
import "./bootstrap";

const legacyLoader = require("./legacy");

export type OrderID<T = string> = T | number;
type InternalToken = string;

export enum OrderState {
  Pending = "PENDING",
  Done = "DONE",
}

export interface Repository<T> extends BaseStore {
  readonly count: number;
  findById(id: OrderID<T>): Promise<T | null>;
}

export namespace Helpers {
  export function formatKey(prefix: string, id: OrderID): string {
    return `${prefix}:${String(id)}`;
  }
}

function sealed(target: Function): void {
  void target;
}

class BaseService {
  protected started = false;
  boot(): void {
    this.started = true;
  }
}

/** Coordinates order lookups and lazy plugin loading. */
@sealed
export class OrderService<T> extends BaseService implements Repository<T> {
  #apiSecret = "internal";
  readonly count: number = 0;

  constructor(private readonly cfg: Config, private logger: Logger) {
    super();
  }

  #signPayload(raw: string): string {
    return `${this.#apiSecret}:${raw}`;
  }

  async findById(id: OrderID<T>): Promise<T | null> {
    if (!id) {
      return null;
    }
    const signed = this.#signPayload(normalize(String(id)));
    const dyn = await import("./lazy_plugin");
    const client = new DefaultClient(this.cfg);
    return dyn.load<T>(client, signed) ?? null;
  }
}

export const buildKey = (scope: string, ...parts: string[]): string => {
  return parts.length > 0 && scope !== "" ? Helpers.formatKey(scope, parts[0]) : "default";
};

export const evaluateBatch = (items: string[]): number => {
  let total = 0;
  for (const item of items) {
    try {
      switch (item) {
        case "skip":
          break;
        default:
          total += item.length > 0 ? 1 : 0;
      }
    } catch (err) {
      total -= 1;
    }
  }
  return total;
};

export { normalizeId as publicNormalize } from "./client";
export * from "./shared";
export default BaseService;
