import type { NextConfig } from "next";

const config: NextConfig = {
  // The dashboard is one server (Node) in front of the Go admin API; nothing here is exported statically.
  poweredByHeader: false,
};

export default config;
