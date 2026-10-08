import { Panel } from "./ui";

/** Stands in for a screen that a later phase builds. It shows no invented data. */
export function ComingSoon({ title, phase, children }: { title: string; phase: number; children: string }) {
  return (
    <>
      <div className="head">
        <h1 className="h1">{title}</h1>
      </div>
      <Panel className="soon">
        <h2>Not built yet</h2>
        <p>{children}</p>
        <p className="mute">This screen arrives in phase {phase}.</p>
      </Panel>
    </>
  );
}
