import fs from "node:fs";
import { designCss, driftAgainstDesignCss, generate, outCss, readTokens } from "./tokens-lib";

const css = generate(readTokens());

if (process.argv.includes("--check")) {
  const problems: string[] = [];
  if (!fs.existsSync(outCss) || fs.readFileSync(outCss, "utf8") !== css) {
    problems.push("src/styles/tokens.css is out of date: run `npm run tokens`");
  }
  problems.push(...driftAgainstDesignCss(css, fs.readFileSync(designCss, "utf8")));
  if (problems.length) {
    console.error(problems.join("\n"));
    process.exit(1);
  }
  console.log("tokens: no drift");
} else {
  fs.writeFileSync(outCss, css);
  console.log("wrote", outCss);
}
