pragma ComponentBehavior: Bound

import QtQuick
import QtQuick.Layouts
import QtQuick.Controls as QQC2
import org.kde.plasma.plasmoid
import org.kde.plasma.core as PlasmaCore
import org.kde.plasma.components as PlasmaComponents3
import org.kde.plasma.extras as PlasmaExtras
import org.kde.kirigami as Kirigami

PlasmoidItem {
    id: root

    property var fleetStatus: ({
        online: false,
        working_count: 0,
        review_count: 0,
        queued_count: 0,
        total_spend_usd: 0.0,
        spaces: [],
        tasks: []
    })

    readonly property bool inPanel: [
        PlasmaCore.Types.TopEdge,
        PlasmaCore.Types.RightEdge,
        PlasmaCore.Types.BottomEdge,
        PlasmaCore.Types.LeftEdge,
    ].includes(Plasmoid.location)

    Plasmoid.icon: {
        if (!fleetStatus.online) return "state-offline";
        if (fleetStatus.working_count > 0) return "system-run";
        if (fleetStatus.review_count > 0) return "document-edit";
        return "applications-development";
    }

    Plasmoid.status: fleetStatus.online ? PlasmaCore.Types.ActiveStatus : PlasmaCore.Types.PassiveStatus

    Plasmoid.title: "Fleet"
    toolTipMainText: "Fleet Agent Cockpit"
    toolTipSubText: {
        if (!fleetStatus.online) return "Fleet daemon offline";
        let parts = [];
        if (fleetStatus.working_count > 0) parts.push(fleetStatus.working_count + " working");
        if (fleetStatus.review_count > 0) parts.push(fleetStatus.review_count + " review");
        if (parts.length === 0) parts.push("idle");
        parts.push("$" + (fleetStatus.total_spend_usd || 0).toFixed(2));
        return parts.join(" • ");
    }

    function reloadStatus() {
        var xhr = new XMLHttpRequest();
        xhr.onreadystatechange = function() {
            if (xhr.readyState === XMLHttpRequest.DONE) {
                if (xhr.status === 200 || xhr.status === 0) {
                    try {
                        root.fleetStatus = JSON.parse(xhr.responseText);
                    } catch (e) {
                        // ignore parse errors
                    }
                } else {
                    root.fleetStatus = { online: false, working_count: 0, review_count: 0, queued_count: 0, total_spend_usd: 0, spaces: [], tasks: [] };
                }
            }
        };
        xhr.open("GET", "http://127.0.0.1:2978/status");
        xhr.send();
    }

    Timer {
        id: statusTimer
        interval: 2000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: root.reloadStatus()
    }

    onExpandedChanged: {
        if (root.expanded) {
            root.reloadStatus();
        }
    }

    compactRepresentation: CompactRepresentation {}
    fullRepresentation: FullRepresentation {}
}
