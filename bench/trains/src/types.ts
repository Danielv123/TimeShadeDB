// The evaluator's view of train data and of a train renderer. A renderer is whatever
// turns samples into a drawn train: the illisis viewer's code (baseline), simple
// reference interpolators, or the v3 viewer's movers module (WP8).

/** One mover sample or gone, in map-clock milliseconds and world tiles. */
export interface TrainSample {
  t: number;
  /** Game tick, when the data has one (v3 capture); null for imported illisis data. */
  tick: number | null;
  x: number;
  y: number;
  /** Velocity in tiles per second; 0 when unknown (v3 does not store it). */
  vx: number;
  vy: number;
  /** Orientation 0..1 (0 north, 0.25 east), or null when unknown. */
  h: number | null;
  gone: boolean;
  /** Carriage count, kinds from the leading end, leading locomotive colour. */
  n: number;
  kinds: string;
  colour: string;
  stop?: boolean;
  /** The leading end switched at this sample. */
  es?: boolean;
  /** Every carriage as [x, y, orientation], when the capture sent them. */
  car?: [number, number, number][];
}

export interface JourneyMeta {
  id: string;
  force: string;
  name: string;
}

/** A journey: one train's samples in time order, with its handoff windows. */
export interface Journey extends JourneyMeta {
  samples: TrainSample[];
  /**
   * Instance crossings: [gone time, next sample time] for each gone followed by a sample
   * of the same journey within 10 s. Metrics exclude a 1 s margin around these.
   */
  handoffs: [number, number][];
}

/** What a renderer draws at time t. */
export interface Pose {
  /** The leading carriage centre. */
  x: number;
  y: number;
  /** Every drawn carriage centre, the leading one first. */
  cars: { x: number; y: number }[];
}

export interface TrainSession {
  /** Called in time order. Before pose(t), every sample known at t has been fed. */
  feed(s: TrainSample): void;
  /** The drawn train at t (non-decreasing), or undefined when the train is not shown. */
  pose(t: number): Pose | undefined;
}

export interface TrainRenderer {
  name: string;
  session(meta: JourneyMeta): TrainSession;
}
