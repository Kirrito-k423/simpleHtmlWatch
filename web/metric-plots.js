'use strict';
// Shared browser/Node geometry. Missing buckets and temporal gaps split paths.
const MetricPlots = (() => {
  const colors = ['lime','blue','amber','pink','violet','teal','orange','gray','cyan','red','olive','white'];
  function geometry(series, after, before, step = 1, fixedMax = 0) {
    const left = 56, right = 748, top = 12, bottom = 150;
    const values = series.flatMap(item => item.points.filter(p => p.at >= after && p.at <= before && Number.isFinite(p.value)).map(p => p.value));
    const max = Math.max(fixedMax, ...values, 1);
    const min = Math.min(0, ...values);
    const x = at => left + (at - after) / Math.max(1, before - after) * (right - left);
    const y = value => bottom - (value - min) / Math.max(1, max - min) * (bottom - top);
    const paths = series.map((item, index) => {
      let continuous = false, previous = null, path = '';
      for (const point of item.points) {
        if (point.at < after || point.at > before) continue;
        if (!Number.isFinite(point.value)) { continuous = false; previous = point.at; continue; }
        if (previous !== null && point.at - previous > Math.max(1, step) * 1.5) continuous = false;
        path += `${continuous ? 'L' : 'M'}${x(point.at).toFixed(2)},${y(point.value).toFixed(2)} `;
        continuous = true; previous = point.at;
      }
      return {path:path.trim(), color:colors[index % colors.length]};
    });
    return {paths, min, max, left, right, top, bottom, x, hasData:values.length > 0};
  }
  function nearest(points, at, step = 1) {
    const point = points.reduce((best, p) => !best || Math.abs(p.at - at) < Math.abs(best.at - at) ? p : best, null);
    return point && Math.abs(point.at - at) <= Math.max(1, step) * .55 ? point : null;
  }
  function zoom(after, before, factor, anchor = .5) {
    const span = Math.max(60, Math.min(7 * 86400, (before - after) * factor));
    anchor = Math.max(0, Math.min(1, anchor));
    const center = after + (before - after) * anchor;
    return {after:Math.max(1, Math.round(center - span * anchor)), before:Math.round(center + span * (1 - anchor))};
  }
  return {colors, geometry, nearest, zoom};
})();
if (typeof module !== 'undefined') module.exports = MetricPlots;
