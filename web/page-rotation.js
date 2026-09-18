'use strict';

// The dashboard owns rendering; this clock advances at most one page per tick.
class PageRotation {
  constructor() { this.seconds = 0; this.deadline = null; }
  setSeconds(seconds, now = performance.now()) {
    this.seconds = [10, 15, 30, 60].includes(seconds) ? seconds : 0;
    this.reset(now);
  }
  reset(now = performance.now()) { this.deadline = this.seconds ? now + this.seconds * 1000 : null; }
  advance(page, pageCount, blocked, now = performance.now()) {
    if (!this.seconds || pageCount <= 1 || blocked) { this.reset(now); return page; }
    if (this.deadline === null) this.reset(now);
    if (now < this.deadline) return page;
    this.reset(now);
    return (page + 1) % pageCount;
  }
  remaining(now = performance.now()) { return Math.max(0, Math.ceil(((this.deadline ?? now) - now) / 1000)); }
}
if (typeof module !== 'undefined') module.exports = {PageRotation};
