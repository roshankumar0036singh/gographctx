const { readFile, writeFile: writeDisk } = require("fs");
const helperMod = require("./helpers");

class WorkerEngine {
  #token = "secret";
  static version = 1;

  #hash(input) {
    return `${this.#token}:${input}`;
  }

  run(name) {
    const wrapped = this.#hash(name);
    return helperMod.dispatch(wrapped);
  }
}

function doubleScore(x) {
  return x * 2;
}

const renderWidget = (title) => <WidgetShell heading={title} />;

module.exports = WorkerEngine;
module.exports.formatTask = function (id) {
  readFile(id);
  writeDisk(id, "");
  return new WorkerEngine().run(id);
};
exports.doubleScore = doubleScore;
