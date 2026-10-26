import { DatabaseSync } from "node:sqlite";
import { chmodSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import {
  defaultRecipes,
  hash,
  type InputRecord,
  type Job,
  terminal,
} from "./contracts.js";
export class Store {
  db: DatabaseSync;
  constructor(directory: string) {
    mkdirSync(directory, { recursive: true, mode: 0o700 });
    chmodSync(directory, 0o700);
    const file = join(directory, "runtime.sqlite");
    this.db = new DatabaseSync(file);
    chmodSync(file, 0o600);
    this.db
      .exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;
      CREATE TABLE IF NOT EXISTS jobs (ordinal INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT UNIQUE NOT NULL, namespace TEXT NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS jobs_namespace ON jobs(namespace, ordinal);
      CREATE TABLE IF NOT EXISTS events (job_id TEXT NOT NULL REFERENCES jobs(job_id), sequence INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(job_id,sequence));
      CREATE TABLE IF NOT EXISTS idempotency (namespace TEXT NOT NULL, key TEXT NOT NULL, digest TEXT NOT NULL, job_id TEXT NOT NULL REFERENCES jobs(job_id), PRIMARY KEY(namespace,key));
      CREATE TABLE IF NOT EXISTS models (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS recipes (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS adapters (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS inputs (input_id TEXT PRIMARY KEY, namespace TEXT NOT NULL, data TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS inputs_namespace ON inputs(namespace, input_id);`);
    for (const recipe of defaultRecipes)
      this.db
        .prepare("INSERT OR IGNORE INTO recipes VALUES (?,?)")
        .run(
          recipe.recipe_id,
          JSON.stringify({ ...recipe, digest: hash(recipe) }),
        );
  }
  insertInput(record: InputRecord) {
    this.db
      .prepare("INSERT INTO inputs(input_id,namespace,data) VALUES (?,?,?)")
      .run(record.input_id, record.namespace, JSON.stringify(record));
  }
  input(input_id: string, namespace?: string): InputRecord | undefined {
    const row = namespace
      ? this.db
          .prepare("SELECT data FROM inputs WHERE input_id=? AND namespace=?")
          .get(input_id, namespace)
      : this.db.prepare("SELECT data FROM inputs WHERE input_id=?").get(input_id);
    return row ? JSON.parse(row.data as string) : undefined;
  }
  inputs(namespace?: string): InputRecord[] {
    return (
      namespace
        ? this.db
            .prepare("SELECT data FROM inputs WHERE namespace=? ORDER BY rowid")
            .all(namespace)
        : this.db.prepare("SELECT data FROM inputs ORDER BY rowid").all()
    ).map((r: any) => JSON.parse(r.data));
  }
  deleteInput(input_id: string) {
    this.db.prepare("DELETE FROM inputs WHERE input_id=?").run(input_id);
  }
  transaction<T>(fn: () => T): T {
    this.db.exec("BEGIN IMMEDIATE");
    try {
      const result = fn();
      this.db.exec("COMMIT");
      return result;
    } catch (e) {
      this.db.exec("ROLLBACK");
      throw e;
    }
  }
  all(state?: string): Job[] {
    return (
      state
        ? this.db
            .prepare("SELECT data FROM jobs WHERE state=? ORDER BY ordinal")
            .all(state)
        : this.db.prepare("SELECT data FROM jobs ORDER BY ordinal").all()
    ).map((r: any) => JSON.parse(r.data));
  }
  get(job_id: string, namespace?: string): Job | undefined {
    const row = namespace
      ? this.db
          .prepare("SELECT data FROM jobs WHERE job_id=? AND namespace=?")
          .get(job_id, namespace)
      : this.db.prepare("SELECT data FROM jobs WHERE job_id=?").get(job_id);
    return row ? JSON.parse(row.data as string) : undefined;
  }
  insert(job: Job) {
    this.db
      .prepare("INSERT INTO jobs(job_id,namespace,state,data) VALUES (?,?,?,?)")
      .run(job.job_id, job.namespace, job.state, JSON.stringify(job));
  }
  save(job: Job) {
    job.updated_at = new Date().toISOString();
    this.db
      .prepare("UPDATE jobs SET state=?,data=? WHERE job_id=?")
      .run(job.state, JSON.stringify(job), job.job_id);
  }
  event(job: Job, type: string, data: any) {
    const sequence = Number(
      (
        this.db
          .prepare(
            "SELECT COALESCE(MAX(sequence),0)+1 AS n FROM events WHERE job_id=?",
          )
          .get(job.job_id) as any
      ).n,
    );
    const event = {
      job_id: job.job_id,
      sequence,
      timestamp: new Date().toISOString(),
      type,
      data,
    };
    this.db
      .prepare("INSERT INTO events VALUES (?,?,?)")
      .run(job.job_id, sequence, JSON.stringify(event));
    return event;
  }
  events(job_id: string, after = 0): any[] {
    return this.db
      .prepare(
        "SELECT data FROM events WHERE job_id=? AND sequence>? ORDER BY sequence",
      )
      .all(job_id, after)
      .map((r: any) => JSON.parse(r.data));
  }
  finish(job: Job, state: string, error?: any) {
    const current = this.get(job.job_id)!;
    if (terminal(current.state)) return;
    this.transaction(() => {
      job.state = state;
      job.error = error;
      job.stage = null;
      this.save(job);
      if (error) this.event(job, "error", error);
      this.event(job, "job_state", { state });
    });
  }
  registry(kind: "models" | "recipes" | "adapters", id?: string): any {
    if (id) {
      const row = this.db
        .prepare(`SELECT data FROM ${kind} WHERE id=?`)
        .get(id);
      return row ? JSON.parse(row.data as string) : undefined;
    }
    return this.db
      .prepare(`SELECT data FROM ${kind} ORDER BY id`)
      .all()
      .map((r: any) => JSON.parse(r.data));
  }
  register(kind: "models" | "recipes" | "adapters", id: string, data: any) {
    this.db
      .prepare(
        `INSERT INTO ${kind} VALUES (?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`,
      )
      .run(id, JSON.stringify(data));
  }
}
