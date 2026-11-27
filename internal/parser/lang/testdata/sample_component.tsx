import React from "react";
import { Panel, UI } from "./ui";

type BadgeProps = {
  title: string;
  active?: boolean;
};

/** Renders an order summary card. */
export const OrderCard: React.FC<BadgeProps> = ({ title, active = true }) => {
  return (
    <Panel>
      <UI.Header label={title} />
      <div className={active ? "on" : "off"} />
    </Panel>
  );
};
